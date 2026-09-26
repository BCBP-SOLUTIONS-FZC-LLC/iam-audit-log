//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
)

func directEntry(t *testing.T, tenant, key string) domain.AuditEntry {
	t.Helper()
	e, err := domain.BuildDirectWriteEntry(domain.DirectWriteCommand{
		IdempotencyKey: key, TenantID: tenant, EntryType: "config.tenant_setting.changed", Action: "update",
		Actor:      domain.ActorRef{Type: domain.ActorUser, ID: uuid.NewString(), Display: "asha@acme.example"},
		Target:     &domain.TargetRef{Type: "tenant_setting", ID: "mfa_freshness_seconds"},
		OccurredAt: time.Now().UTC().Truncate(time.Microsecond), SourceService: "iam-org-membership",
		SourceEventType: "TenantSettingChanged", IPAddress: "203.0.113.7", UserAgent: "om/1.0", TraceID: "4bf92f3577b34da6a3ce929d0e0e4736",
		Metadata: json.RawMessage(`{"setting":"mfa_freshness_seconds","from":900,"to":1800}`),
	}, 8192)
	require.NoError(t, err)
	id, _ := uuid.NewV7()
	e.ID = id.String()
	return e
}

func rowCount(t *testing.T, db *testDB, key string) int {
	t.Helper()
	var n int
	require.NoError(t, db.raw.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE source_event_id = $1`, key).Scan(&n))
	return n
}

// §5.4 server-side processing on the real audit_app role: the row is
// persisted with the derived tier, direct_write mode and NULL topic; a
// replay of the key returns the same row (200) and writes nothing (AL-INV-4).
func TestIngest_CreateThenReplay_ALINV4(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	repo := pgadapter.NewAuditRepository(db.appPool)
	ctx := context.Background()
	key := uuid.NewString()
	e := directEntry(t, tenantA, key)

	stored, created, err := repo.Append(ctx, e, domain.ConsumerDirectWrite)
	require.NoError(t, err)
	assert.True(t, created)
	assert.False(t, stored.RecordedAt.IsZero())

	var topicNull bool
	var mode, tier, from string
	require.NoError(t, db.raw.QueryRow(ctx, `SELECT source_topic IS NULL, ingest_mode::text, retention_tier::text, metadata->>'from'
		FROM audit_events WHERE source_event_id = $1`, key).Scan(&topicNull, &mode, &tier, &from))
	assert.True(t, topicNull)
	assert.Equal(t, "direct_write", mode)
	assert.Equal(t, "security_3y", tier)
	assert.Equal(t, "900", from, "jsonb metadata round-trips under PgBouncer simple protocol (gap 28)")

	retry := directEntry(t, tenantA, key) // a retry mints a fresh id and time
	again, created, err := repo.Append(ctx, retry, domain.ConsumerDirectWrite)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, stored.ID, again.ID)
	assert.Equal(t, e.Actor, again.Actor)
	assert.Equal(t, e.Target, again.Target)
	assert.Equal(t, "203.0.113.7", again.IPAddress)
	assert.JSONEq(t, string(e.Metadata), string(again.Metadata))
	assert.Equal(t, 1, rowCount(t, db, key))
}

// AL-INV-4: once the 8-day ledger row is pruned, the
// (source_event_id, occurred_at) backstop still dedups a very late replay.
func TestIngest_ReplayAfterLedgerPruneHitsBackstop_ALINV4(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	repo := pgadapter.NewAuditRepository(db.appPool)
	ctx := context.Background()
	e := directEntry(t, tenantA, uuid.NewString())
	first, _, err := repo.Append(ctx, e, domain.ConsumerDirectWrite)
	require.NoError(t, err)

	_, err = db.raw.Exec(ctx, `DELETE FROM processed_events WHERE event_id = $1`, e.SourceEventID)
	require.NoError(t, err)

	late := e
	late.ID = uuid.NewString()
	again, created, err := repo.Append(ctx, late, domain.ConsumerDirectWrite)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, first.ID, again.ID)
	assert.Equal(t, 1, rowCount(t, db, e.SourceEventID))
}

// AL-INV-3: a key already used by tenant A, replayed by tenant B, is
// rejected — the replay path never returns (or aliases) A's entry.
func TestIngest_KeyReuseAcrossTenantsDoesNotLeak_ALINV3(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	repo := pgadapter.NewAuditRepository(db.appPool)
	ctx := context.Background()
	key := uuid.NewString()
	_, _, err := repo.Append(ctx, directEntry(t, tenantA, key), domain.ConsumerDirectWrite)
	require.NoError(t, err)

	got, created, err := repo.Append(ctx, directEntry(t, tenantB, key), domain.ConsumerDirectWrite)
	require.Error(t, err)
	var de *domain.Error
	require.True(t, errors.As(err, &de))
	assert.Equal(t, domain.ErrInvalidRequest, de.Code)
	assert.False(t, created)
	assert.Empty(t, got.ID)
	assert.Equal(t, 1, rowCount(t, db, key))
}

// §4.3 / §10.2: the row is bound to the BODY tenant, whatever tenant the
// caller's own context carried — and it is visible only to that tenant.
func TestIngest_BindsBodyTenantNotCallerTenant_ALINV3(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	repo := pgadapter.NewAuditRepository(db.appPool)
	callerCtx := withTenant(context.Background(), tenantB) // e.g. x-tenant-id of the producer
	e := directEntry(t, tenantA, uuid.NewString())
	_, created, err := repo.Append(callerCtx, e, domain.ConsumerDirectWrite)
	require.NoError(t, err)
	require.True(t, created)
	assert.Equal(t, 1, countVisible(t, withTenant(context.Background(), tenantA), db.appPool))
	assert.Equal(t, 0, countVisible(t, withTenant(context.Background(), tenantB), db.appPool))
}

// AL-D14: the platform_tenant sentinel is accepted like any tenant id; an
// anonymous actor persists with a NULL actor_id (AL-D11).
func TestIngest_PlatformTenantAndAnonymousActor(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	repo := pgadapter.NewAuditRepository(db.appPool)
	ctx := context.Background()
	e := directEntry(t, platformTenant, uuid.NewString())
	e.Actor = domain.ActorRef{Type: domain.ActorAnonymous}
	e.Target, e.IPAddress = nil, ""
	_, created, err := repo.Append(ctx, e, domain.ConsumerDirectWrite)
	require.NoError(t, err)
	require.True(t, created)

	again, _, err := repo.Append(ctx, e, domain.ConsumerDirectWrite)
	require.NoError(t, err)
	assert.Equal(t, domain.ActorAnonymous, again.Actor.Type)
	assert.Empty(t, again.Actor.ID)
	assert.Nil(t, again.Target)
	assert.Empty(t, again.IPAddress)
}

// AL-INV-4 under concurrency: many replicas/retries racing the same key
// produce exactly one row and every caller sees the same id.
func TestIngest_ConcurrentSameKeyYieldsOneRow_ALINV4(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	repo := pgadapter.NewAuditRepository(db.appPool)
	key := uuid.NewString()
	const n = 8
	var wg sync.WaitGroup
	ids := make([]string, n)
	createdCount := 0
	var mu sync.Mutex
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stored, created, err := repo.Append(context.Background(), directEntry(t, tenantA, key), domain.ConsumerDirectWrite)
			assert.NoError(t, err)
			mu.Lock()
			defer mu.Unlock()
			ids[i] = stored.ID
			if created {
				createdCount++
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, 1, createdCount)
	for _, id := range ids {
		assert.Equal(t, ids[0], id)
	}
	assert.Equal(t, 1, rowCount(t, db, key))
}
