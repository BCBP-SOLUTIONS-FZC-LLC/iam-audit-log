//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
)

// Decision D-18 (gap 37): a row about an already-erased subject is redacted
// BEFORE insert (ingest-time check against redacted_subjects), and the daily
// sweep_redactions() re-check catches any row that slipped past it.

// lateEntry is a bus-shaped entry about a subject that arrives after the
// subject's redaction finished.
func lateEntry(tenant, entryType string, tier domain.RetentionTier, actor domain.ActorRef, target *domain.TargetRef) domain.AuditEntry {
	id, _ := uuid.NewV7()
	return domain.AuditEntry{
		ID: id.String(), OccurredAt: time.Now().UTC().Truncate(time.Microsecond), TenantID: tenant,
		EntryType: entryType, Action: "update", Actor: actor, Target: target,
		SourceService: "iam-user-profile", SourceTopic: "iam.user.events", SourceEventType: "UserUpdated",
		SourceEventID: uuid.NewString(), RetentionTier: tier, IngestMode: domain.IngestBus,
		Metadata: json.RawMessage(`{"email":"asha@acme.example","note":"late"}`),
	}
}

func assertIngestMarker(t *testing.T, r storedRow, taskID string) {
	t.Helper()
	assert.Equal(t, true, r.Metadata["_redacted"], r.RawMetadata)
	assert.Equal(t, true, r.Metadata["_redacted_on_ingest"], r.RawMetadata)
	assert.Equal(t, taskID, r.Metadata["_redaction_task_id"], r.RawMetadata)
	assert.NotEmpty(t, r.Metadata["_redacted_at"])
	assert.NotContains(t, r.RawMetadata, "asha@acme.example")
	assert.Len(t, r.Metadata, 4, "marker + _redacted_on_ingest replaces metadata whole")
}

type erasedRow struct {
	TaskID      string
	CompletedAt time.Time
}

func readErased(t *testing.T, db *testDB, tenant, subject string) (erasedRow, bool) {
	t.Helper()
	var r erasedRow
	err := db.raw.QueryRow(context.Background(),
		`SELECT task_id::text, redaction_completed_at FROM redacted_subjects WHERE tenant_id = $1 AND subject_id = $2`,
		tenant, subject).Scan(&r.TaskID, &r.CompletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, false
	}
	require.NoError(t, err)
	return r, true
}

func TestRedaction_LateArrivalRedactedOnIngest_D18(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	ctx := context.Background()
	repo := pgadapter.NewAuditRepository(db.appPool)
	subject := uuid.NewString()
	taskID, out := scheduleAndApply(t, db, tenantA, subject)
	require.Equal(t, domain.RedactionApplied, out.Status) // the trigger row itself

	admin := domain.ActorRef{Type: domain.ActorUser, ID: uuid.NewString(), Display: "admin@acme.example"}
	subj := domain.ActorRef{Type: domain.ActorUser, ID: subject, Display: "asha@acme.example"}

	// Subject as actor: marker + actor_display cleared; the Append result
	// reflects the redaction too.
	e := lateEntry(tenantA, "user.updated", domain.TierSecurity3y, subj, nil)
	stored, created, err := repo.Append(ctx, e, "bus:user-audit-q")
	require.NoError(t, err)
	require.True(t, created)
	assert.Empty(t, stored.Actor.Display)
	assert.Contains(t, string(stored.Metadata), `"_redacted_on_ingest":true`)
	r := readRow(t, db, e.ID)
	assertIngestMarker(t, r, taskID)
	assert.Nil(t, r.ActorDisplay)
	require.NotNil(t, r.ActorID)
	assert.Equal(t, subject, *r.ActorID, "ids are kept (§15.5)")

	// Subject as user target: marker; the admin actor's display kept.
	e = lateEntry(tenantA, "user.updated", domain.TierSecurity3y, admin, &domain.TargetRef{Type: "user", ID: subject})
	_, _, err = repo.Append(ctx, e, "bus:user-audit-q")
	require.NoError(t, err)
	r = readRow(t, db, e.ID)
	assertIngestMarker(t, r, taskID)
	require.NotNil(t, r.ActorDisplay)
	assert.Equal(t, "admin@acme.example", *r.ActorDisplay)
	require.NotNil(t, r.TargetID)
	assert.Equal(t, subject, *r.TargetID)

	// Untouched: compliance_7y (AL-INV-7), another tenant, another subject,
	// a non-user target carrying the same id string.
	untouched := []domain.AuditEntry{
		lateEntry(tenantA, "tender.section.approved", domain.TierCompliance7y, subj, nil),
		lateEntry(tenantB, "user.updated", domain.TierSecurity3y, subj, nil),
		lateEntry(tenantA, "user.updated", domain.TierSecurity3y, admin, &domain.TargetRef{Type: "user", ID: uuid.NewString()}),
		lateEntry(tenantA, "user.updated", domain.TierSecurity3y, admin, &domain.TargetRef{Type: "tender", ID: subject}),
	}
	for i, u := range untouched {
		s, _, err := repo.Append(ctx, u, "bus:user-audit-q")
		require.NoError(t, err, i)
		r := readRow(t, db, u.ID)
		assert.Contains(t, r.RawMetadata, "asha@acme.example", "case %d must be stored verbatim", i)
		assert.NotContains(t, r.RawMetadata, "_redacted", "case %d", i)
		assert.JSONEq(t, string(u.Metadata), string(s.Metadata), "case %d", i)
		require.NotNil(t, r.ActorDisplay, i)
		assert.Equal(t, u.Actor.Display, *r.ActorDisplay, "case %d", i)
	}
}

// Every finished task (applied, not_applicable, missed) records the subject;
// a later task for the same subject replaces task_id and completed_at.
func TestRedaction_RedactedSubjectsUpserted_D18(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	ctx := context.Background()
	repo := pgadapter.NewAuditRepository(db.appPool)

	applied := uuid.NewString()
	taskID, out := scheduleAndApply(t, db, tenantA, applied)
	require.Equal(t, domain.RedactionApplied, out.Status)
	got, ok := readErased(t, db, tenantA, applied)
	require.True(t, ok, "applied records the subject")
	assert.Equal(t, taskID, got.TaskID)

	// not_applicable: the trigger row names another user, so nothing matches.
	na := uuid.NewString()
	e := userDeletedEntry(tenantA, uuid.NewString())
	req := redactionRequest(e, na)
	_, _, _, err := repo.AppendWithRedaction(ctx, e, "bus:user-audit-q", req)
	require.NoError(t, err)
	out, err = repo.ApplyRedaction(ctx, req.TaskID)
	require.NoError(t, err)
	require.Equal(t, domain.RedactionNotApplicable, out.Status)
	got, ok = readErased(t, db, tenantA, na)
	require.True(t, ok, "not_applicable records the subject")
	assert.Equal(t, req.TaskID, got.TaskID)

	missed := uuid.NewString()
	seedManifest(t, db, tenantA, "audit_events_2019_01", "security_3y", missed)
	missedTask, out := scheduleAndApply(t, db, tenantA, missed)
	require.Equal(t, domain.RedactionMissed, out.Status)
	got, ok = readErased(t, db, tenantA, missed)
	require.True(t, ok, "missed records the subject")
	assert.Equal(t, missedTask, got.TaskID)

	// A second task for the applied subject replaces task_id / completed_at.
	first, _ := readErased(t, db, tenantA, applied)
	second, _ := scheduleAndApply(t, db, tenantA, applied)
	got, _ = readErased(t, db, tenantA, applied)
	assert.Equal(t, second, got.TaskID)
	assert.True(t, !got.CompletedAt.Before(first.CompletedAt))
	var n int
	require.NoError(t, db.raw.QueryRow(ctx, `SELECT count(*) FROM redacted_subjects WHERE subject_id = $1`, applied).Scan(&n))
	assert.Equal(t, 1, n)
}

// redacted_subjects is RLS-scoped: audit_app reads only under the matching
// tenant GUC (fail-closed without one) and cannot write; the reconciler can.
func TestRedaction_RedactedSubjectsGrantsAndRLS_D18(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	ctx := context.Background()
	assert.Equal(t, []string{"SELECT"}, tableGrants(t, db, "audit_app", "redacted_subjects"))
	assert.Equal(t, []string{"INSERT", "SELECT", "UPDATE"}, tableGrants(t, db, "audit_reconciler", "redacted_subjects"))

	subject := uuid.NewString()
	scheduleAndApply(t, db, tenantA, subject)

	count := func(c context.Context) int {
		var n int
		require.NoError(t, inTx(c, db.appPool, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM redacted_subjects`).Scan(&n)
		}))
		return n
	}
	assert.Equal(t, 1, count(withTenant(ctx, tenantA)))
	assert.Equal(t, 0, count(withTenant(ctx, tenantB)))
	assert.Equal(t, 0, count(ctx), "no GUC → no rows (fail-closed)")

	for _, stmt := range []string{
		`INSERT INTO redacted_subjects (tenant_id, subject_id, task_id, redaction_completed_at) VALUES ($1, gen_random_uuid(), gen_random_uuid(), now())`,
		`UPDATE redacted_subjects SET redaction_completed_at = now() WHERE tenant_id = $1`,
	} {
		err := inTx(withTenant(ctx, tenantA), db.appPool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, stmt, tenantA)
			return err
		})
		require.Error(t, err, stmt)
		assert.Contains(t, err.Error(), "permission denied", stmt)
	}
	require.NoError(t, inTx(ctx, db.reconPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO redacted_subjects (tenant_id, subject_id, task_id, redaction_completed_at)
			VALUES ($1, gen_random_uuid(), gen_random_uuid(), now())`, tenantB)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE redacted_subjects SET redaction_completed_at = now() WHERE tenant_id = $1`, tenantB)
		return err
	}), "the reconciler writes redacted_subjects")
}

// sweep_redactions() re-redacts a security_3y row that bypassed the ingest
// check, for subjects erased within the window only; compliance_7y is never
// touched; a second sweep is a no-op; the window is bounded.
func TestRedaction_SweepFixesBypassedRows_D18(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	ctx := context.Background()
	sweeper := pgadapter.NewRedactionRepository(db.reconPool)

	recent := uuid.NewString()
	recentTask, _ := scheduleAndApply(t, db, tenantA, recent)
	old := uuid.NewString()
	scheduleAndApply(t, db, tenantA, old)
	_, err := db.raw.Exec(ctx, `UPDATE redacted_subjects SET redaction_completed_at = now() - interval '100 days'
		WHERE tenant_id = $1 AND subject_id = $2`, tenantA, old)
	require.NoError(t, err)

	// Rows inserted as superuser — they never went through the ingest check.
	bypassed := seedRedactRow(t, db, redactRow{Tenant: tenantA, Tier: "security_3y", ActorID: recent, ActorDisplay: "asha@acme.example"})
	compliance := seedRedactRow(t, db, redactRow{Tenant: tenantA, Tier: "compliance_7y", ActorID: recent, ActorDisplay: "asha@acme.example"})
	outside := seedRedactRow(t, db, redactRow{Tenant: tenantA, Tier: "security_3y", ActorID: old, ActorDisplay: "old@acme.example"})
	compBefore := readRow(t, db, compliance.ID)

	n, err := sweeper.Sweep(ctx, 90*24*time.Hour)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)

	r := readRow(t, db, bypassed.ID)
	assertMarker(t, r, recentTask)
	assert.Nil(t, r.ActorDisplay)
	compAfter := readRow(t, db, compliance.ID)
	assert.Equal(t, compBefore.RawMetadata, compAfter.RawMetadata, "compliance_7y untouched (AL-INV-7)")
	assert.Equal(t, compBefore.ActorDisplay, compAfter.ActorDisplay)
	assert.Contains(t, readRow(t, db, outside.ID).RawMetadata, "asha@acme.example", "erased outside the window")

	n, err = sweeper.Sweep(ctx, 90*24*time.Hour)
	require.NoError(t, err)
	assert.EqualValues(t, 0, n, "second sweep is a no-op")

	for _, w := range []time.Duration{12 * time.Hour, 401 * 24 * time.Hour} {
		_, err := sweeper.Sweep(ctx, w)
		require.Error(t, err, "window %s", w)
	}
}
