//go:build integration

package postgres_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

const platformTenant = "00000000-0000-0000-0000-0000000000b1"

// inTx runs fn in one pgcommon transaction on pool (GUC bound from ctx).
func inTx(ctx context.Context, pool *pgcommon.Pool, fn func(tx pgx.Tx) error) error {
	return pgcommon.RunInTx(ctx, pool, pgx.TxOptions{}, func(_ context.Context, tx pgx.Tx) error {
		return fn(tx)
	})
}

// eventRow is a minimal valid audit_events row.
type eventRow struct {
	ID, Tenant, SourceID string
	OccurredAt           time.Time
	ActorType            string
	ActorID              any // uuid string or nil
	Tier                 string
	Metadata             string
}

func newEvent(tenant string, at time.Time) eventRow {
	return eventRow{
		ID: uuid.NewString(), Tenant: tenant, SourceID: uuid.NewString(), OccurredAt: at,
		ActorType: "user", ActorID: uuid.NewString(), Tier: "security_3y", Metadata: `{}`,
	}
}

const insertEventSQL = `INSERT INTO audit_events (
    id, occurred_at, tenant_id, entry_type, action, actor_type, actor_id,
    source_service, source_event_type, source_event_id, retention_tier, ingest_mode, metadata)
VALUES ($1, $2, $3, 'user.updated', 'update', $4::audit_actor_type, $5,
    'iam-user-profile', 'UserUpdated', $6, $7::audit_retention_tier, 'bus', $8::jsonb)`

func (e eventRow) args() []any {
	return []any{e.ID, e.OccurredAt, e.Tenant, e.ActorType, e.ActorID, e.SourceID, e.Tier, e.Metadata}
}

// seedEvent inserts e as superuser (bypasses RLS).
func seedEvent(t *testing.T, db *testDB, e eventRow) {
	t.Helper()
	_, err := db.raw.Exec(context.Background(), insertEventSQL, e.args()...)
	require.NoError(t, err)
}

// countVisible counts audit_events rows visible to pool under ctx's GUC.
func countVisible(t *testing.T, ctx context.Context, pool *pgcommon.Pool) int {
	t.Helper()
	var n int
	require.NoError(t, inTx(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_events`).Scan(&n)
	}))
	return n
}

// monthStart returns the first instant (UTC) of the month offset from now.
func monthStart(offset int) time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month()+time.Month(offset), 1, 0, 0, 0, 0, time.UTC)
}

// partitionOf reports which physical partition holds source_event_id.
func partitionOf(t *testing.T, db *testDB, sourceID string) string {
	t.Helper()
	var p string
	require.NoError(t, db.raw.QueryRow(context.Background(),
		`SELECT tableoid::regclass::text FROM audit_events WHERE source_event_id = $1`, sourceID).Scan(&p))
	return p
}

func partitionName(offset int) string {
	return fmt.Sprintf("audit_events_%s", monthStart(offset).Format("2006_01"))
}
