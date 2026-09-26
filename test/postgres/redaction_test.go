//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
)

// Phase 6 — GDPR redaction (LLD §8.7, §15.5; AL-INV-7, AL-INV-12;
// decisions D-1, D-15..D-17).

// redactRow is one seeded audit_events row with the columns redaction reads
// or rewrites.
type redactRow struct {
	ID, Tenant, Tier      string
	ActorID, ActorDisplay any // nil → NULL
	TargetType, TargetID  any
	Metadata              string
}

const insertRedactRowSQL = `INSERT INTO audit_events (
    id, occurred_at, tenant_id, entry_type, action, actor_type, actor_id, actor_display,
    target_type, target_id, source_service, source_event_type, source_event_id,
    retention_tier, ingest_mode, metadata)
VALUES ($1, $2, $3, 'user.updated', 'update', 'user', $4, $5, $6, $7,
    'iam-user-profile', 'UserUpdated', $8, $9::audit_retention_tier, 'bus', $10::jsonb)`

func seedRedactRow(t *testing.T, db *testDB, r redactRow) redactRow {
	t.Helper()
	if r.ID == "" {
		r.ID = uuid.NewString()
	}
	if r.Metadata == "" {
		r.Metadata = `{"email":"asha@acme.example","note":"free text"}`
	}
	_, err := db.raw.Exec(context.Background(), insertRedactRowSQL,
		r.ID, time.Now().UTC().Add(-time.Hour), r.Tenant, r.ActorID, r.ActorDisplay, r.TargetType, r.TargetID,
		uuid.NewString(), r.Tier, r.Metadata)
	require.NoError(t, err)
	return r
}

// storedRow is a row's post-redaction state, read as superuser.
type storedRow struct {
	Metadata     map[string]any
	RawMetadata  string
	ActorID      *string
	ActorDisplay *string
	TargetID     *string
}

func readRow(t *testing.T, db *testDB, id string) storedRow {
	t.Helper()
	var r storedRow
	require.NoError(t, db.raw.QueryRow(context.Background(),
		`SELECT metadata::text, actor_id::text, actor_display, target_id FROM audit_events WHERE id = $1`, id).
		Scan(&r.RawMetadata, &r.ActorID, &r.ActorDisplay, &r.TargetID))
	require.NoError(t, json.Unmarshal([]byte(r.RawMetadata), &r.Metadata))
	return r
}

// userDeletedEntry is the bus-shaped user.deleted entry for subject: the
// iam_system actor and the subject as user target (D-8), security_3y.
func userDeletedEntry(tenant, subject string) domain.AuditEntry {
	id, _ := uuid.NewV7()
	return domain.AuditEntry{
		ID: id.String(), OccurredAt: time.Now().UTC().Truncate(time.Microsecond), TenantID: tenant,
		EntryType: "user.deleted", Action: "deleted",
		Actor:         domain.ActorRef{Type: domain.ActorIAMSystem, ID: domain.IAMSystemActorID},
		Target:        &domain.TargetRef{Type: "user", ID: subject},
		SourceService: "iam-user-profile", SourceTopic: "iam.user.events", SourceEventType: "UserDeleted",
		SourceEventID: uuid.NewString(), RetentionTier: domain.TierSecurity3y, IngestMode: domain.IngestBus,
		Metadata: json.RawMessage(`{"user_id":"` + subject + `","email":"asha@acme.example"}`),
	}
}

func redactionRequest(e domain.AuditEntry, subject string) domain.RedactionRequest {
	return domain.RedactionRequest{
		TaskID: uuid.NewString(), TenantID: e.TenantID, SubjectID: subject,
		TriggerEventType: domain.RedactionTriggerUserDeleted, TriggerSourceEventID: e.SourceEventID,
	}
}

type taskRow struct {
	Status    string
	AppliedAt *time.Time
	Rows      *int64
}

func readTask(t *testing.T, db *testDB, id string) taskRow {
	t.Helper()
	var r taskRow
	require.NoError(t, db.raw.QueryRow(context.Background(),
		`SELECT status::text, applied_at, rows_redacted FROM audit_redaction_tasks WHERE id = $1`, id).
		Scan(&r.Status, &r.AppliedAt, &r.Rows))
	return r
}

// scheduleAndApply ingests a user.deleted for subject with its task and
// applies it, returning the task id and outcome.
func scheduleAndApply(t *testing.T, db *testDB, tenant, subject string) (string, domain.RedactionOutcome) {
	t.Helper()
	ctx := context.Background()
	repo := pgadapter.NewAuditRepository(db.appPool)
	e := userDeletedEntry(tenant, subject)
	req := redactionRequest(e, subject)
	_, created, taskCreated, err := repo.AppendWithRedaction(ctx, e, "bus:user-audit-q", req)
	require.NoError(t, err)
	require.True(t, created)
	require.True(t, taskCreated)
	out, err := repo.ApplyRedaction(ctx, req.TaskID)
	require.NoError(t, err)
	return req.TaskID, out
}

func assertMarker(t *testing.T, r storedRow, taskID string) {
	t.Helper()
	assert.Equal(t, true, r.Metadata["_redacted"], r.RawMetadata)
	assert.Equal(t, taskID, r.Metadata["_redaction_task_id"], r.RawMetadata)
	assert.NotEmpty(t, r.Metadata["_redacted_at"])
	assert.NotContains(t, r.RawMetadata, "asha@acme.example")
	assert.Len(t, r.Metadata, 3, "the marker replaces metadata whole (D-15)")
}

// AL-INV-12 / D-15: UserDeleted commits with its task; the immediate apply
// rewrites the subject's hot security_3y rows — as actor (metadata + own
// actor_display) and as user target (metadata only) — and nothing else.
func TestRedaction_ImmediateOnUserDeleted_ALINV12(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	subject, admin, other := uuid.NewString(), uuid.NewString(), uuid.NewString()

	asActor := seedRedactRow(t, db, redactRow{Tenant: tenantA, Tier: "security_3y", ActorID: subject, ActorDisplay: "asha@acme.example"})
	asActor2 := seedRedactRow(t, db, redactRow{Tenant: tenantA, Tier: "security_3y", ActorID: subject, ActorDisplay: "Asha"})
	asTarget := seedRedactRow(t, db, redactRow{Tenant: tenantA, Tier: "security_3y", ActorID: admin, ActorDisplay: "admin@acme.example",
		TargetType: "user", TargetID: subject})
	compliance := seedRedactRow(t, db, redactRow{Tenant: tenantA, Tier: "compliance_7y", ActorID: subject, ActorDisplay: "asha@acme.example"})
	otherUser := seedRedactRow(t, db, redactRow{Tenant: tenantA, Tier: "security_3y", ActorID: other, ActorDisplay: "bob@acme.example"})
	otherTenant := seedRedactRow(t, db, redactRow{Tenant: tenantB, Tier: "security_3y", ActorID: subject, ActorDisplay: "asha@acme.example"})
	nonUserTarget := seedRedactRow(t, db, redactRow{Tenant: tenantA, Tier: "security_3y", ActorID: admin,
		TargetType: "tenant_setting", TargetID: subject}) // same id string, but not a user target

	taskID, out := scheduleAndApply(t, db, tenantA, subject)
	// 2 actor rows + 1 user-target row + the user.deleted row itself (subject as target).
	assert.Equal(t, domain.RedactionApplied, out.Status)
	assert.EqualValues(t, 4, out.RowsRedacted)

	for _, r := range []redactRow{asActor, asActor2} {
		got := readRow(t, db, r.ID)
		assertMarker(t, got, taskID)
		assert.Nil(t, got.ActorDisplay, "the subject's own actor_display is cleared")
		require.NotNil(t, got.ActorID)
		assert.Equal(t, subject, *got.ActorID, "actor_id (opaque sub) is kept")
	}
	got := readRow(t, db, asTarget.ID)
	assertMarker(t, got, taskID)
	require.NotNil(t, got.ActorDisplay)
	assert.Equal(t, "admin@acme.example", *got.ActorDisplay, "another actor's display is kept")
	require.NotNil(t, got.TargetID)
	assert.Equal(t, subject, *got.TargetID, "target_id is kept")

	for _, r := range []redactRow{compliance, otherUser, otherTenant, nonUserTarget} {
		got := readRow(t, db, r.ID)
		assert.NotContains(t, got.Metadata, "_redacted", "row %s must be untouched", r.ID)
	}

	task := readTask(t, db, taskID)
	assert.Equal(t, "applied", task.Status)
	require.NotNil(t, task.AppliedAt)
	require.NotNil(t, task.Rows)
	assert.EqualValues(t, 4, *task.Rows)
}

// AL-INV-7: compliance_7y rows of the subject are byte-identical after the
// redaction — never selected, not merely retained.
func TestRedaction_Compliance7yUntouched_ALINV7(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	subject := uuid.NewString()
	approval := seedRedactRow(t, db, redactRow{Tenant: tenantA, Tier: "compliance_7y", ActorID: subject,
		ActorDisplay: "asha@acme.example", Metadata: `{"signature_hash": "abc123", "email": "asha@acme.example"}`})
	targeted := seedRedactRow(t, db, redactRow{Tenant: tenantA, Tier: "compliance_7y", ActorID: uuid.NewString(),
		ActorDisplay: "admin@acme.example", TargetType: "user", TargetID: subject})
	before := map[string]storedRow{approval.ID: readRow(t, db, approval.ID), targeted.ID: readRow(t, db, targeted.ID)}

	_, out := scheduleAndApply(t, db, tenantA, subject)
	assert.Equal(t, domain.RedactionApplied, out.Status, "the user.deleted row itself is security_3y")
	for id, b := range before {
		a := readRow(t, db, id)
		assert.Equal(t, b.RawMetadata, a.RawMetadata, "metadata of %s", id)
		assert.Equal(t, b.ActorDisplay, a.ActorDisplay, "actor_display of %s", id)
	}
}

// uq_redaction_trigger: a redelivered UserDeleted (same envelope id, fresh
// task id) creates no second row or task; re-applying a finished task only
// reports it and does not re-stamp rows.
func TestRedaction_RedeliveryNoop(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	ctx := context.Background()
	repo := pgadapter.NewAuditRepository(db.appPool)
	subject := uuid.NewString()
	row := seedRedactRow(t, db, redactRow{Tenant: tenantA, Tier: "security_3y", ActorID: subject, ActorDisplay: "x"})

	e := userDeletedEntry(tenantA, subject)
	first := redactionRequest(e, subject)
	_, created, taskCreated, err := repo.AppendWithRedaction(ctx, e, "bus:user-audit-q", first)
	require.NoError(t, err)
	require.True(t, created && taskCreated)
	out1, err := repo.ApplyRedaction(ctx, first.TaskID)
	require.NoError(t, err)
	stamped := readRow(t, db, row.ID).RawMetadata

	redelivered := e
	id, _ := uuid.NewV7()
	redelivered.ID = id.String()
	second := redactionRequest(redelivered, subject) // new task id, same trigger_source_event_id
	_, created, taskCreated, err = repo.AppendWithRedaction(ctx, redelivered, "bus:user-audit-q", second)
	require.NoError(t, err)
	assert.False(t, created, "ledger dedup (AL-INV-4)")
	assert.False(t, taskCreated, "uq_redaction_trigger makes the redelivery a no-op")

	var n int
	require.NoError(t, db.raw.QueryRow(ctx, `SELECT count(*) FROM audit_redaction_tasks WHERE trigger_source_event_id = $1`,
		e.SourceEventID).Scan(&n))
	assert.Equal(t, 1, n)

	out2, err := repo.ApplyRedaction(ctx, first.TaskID)
	require.NoError(t, err)
	assert.Equal(t, out1, out2, "a finished task only reports its outcome")
	assert.Equal(t, stamped, readRow(t, db, row.ID).RawMetadata, "rows are not re-stamped")
}

// A subject with no security_3y rows at all → not_applicable, 0 rows. The
// trigger row is compliance-free here because the subject is a target of a
// non-user type only.
func TestRedaction_NotApplicable(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	ctx := context.Background()
	repo := pgadapter.NewAuditRepository(db.appPool)
	subject := uuid.NewString()
	seedRedactRow(t, db, redactRow{Tenant: tenantA, Tier: "compliance_7y", ActorID: subject})

	// Schedule the task against a trigger row that does not name the subject
	// as a user target, so no security_3y row matches.
	e := userDeletedEntry(tenantA, uuid.NewString())
	req := redactionRequest(e, subject)
	_, _, taskCreated, err := repo.AppendWithRedaction(ctx, e, "bus:user-audit-q", req)
	require.NoError(t, err)
	require.True(t, taskCreated)
	out, err := repo.ApplyRedaction(ctx, req.TaskID)
	require.NoError(t, err)
	assert.Equal(t, domain.RedactionNotApplicable, out.Status)
	assert.EqualValues(t, 0, out.RowsRedacted)
	assert.Equal(t, "not_applicable", readTask(t, db, req.TaskID).Status)
}

// seedManifest inserts an audit_archive_objects row as superuser (the
// Phase 7 archiver writes these) with subject_ids (D-17).
func seedManifest(t *testing.T, db *testDB, tenant, partition, tier string, subjects ...string) {
	t.Helper()
	_, err := db.raw.Exec(context.Background(), `INSERT INTO audit_archive_objects
		(partition_name, retention_tier, tenant_id, part, period_month, s3_bucket, s3_key, row_count, byte_size,
		 min_occurred_at, max_occurred_at, min_id, max_id, sha256, subject_ids)
		VALUES ($1, $2::audit_retention_tier, $3, 0, '2019-01-01', 'iam-audit-archive', $4, 1, 1,
		        '2019-01-01', '2019-01-31', '00000000-0000-0000-0000-000000000000',
		        'ffffffff-ffff-ffff-ffff-ffffffffffff', 'x', $5::uuid[])`,
		partition, tier, tenant, uuid.NewString(), subjects)
	require.NoError(t, err)
}

// D-17 / AL-Q15: the subject appears in a dropped-partition security_3y
// archive object → missed, with rows_redacted = the hot rows still
// rewritten. Existing partitions, compliance objects and other tenants'
// objects do not count.
func TestRedaction_MissedWhenSubjectArchived_D17(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	subject := uuid.NewString()

	// Noise that must not trigger missed.
	seedManifest(t, db, tenantA, partitionName(0), "security_3y", subject)         // partition still exists (hot)
	seedManifest(t, db, tenantA, "audit_events_2019_02", "compliance_7y", subject) // compliance object
	seedManifest(t, db, tenantB, "audit_events_2019_03", "security_3y", subject)   // other tenant
	seedManifest(t, db, tenantA, "audit_events_2019_04", "security_3y", uuid.NewString())
	seedRedactRow(t, db, redactRow{Tenant: tenantA, Tier: "security_3y", ActorID: subject})
	_, out := scheduleAndApply(t, db, tenantA, subject)
	assert.Equal(t, domain.RedactionApplied, out.Status, "no relevant archived object yet")

	subject2 := uuid.NewString()
	seedManifest(t, db, tenantA, "audit_events_2019_01", "security_3y", uuid.NewString(), subject2)
	hot := seedRedactRow(t, db, redactRow{Tenant: tenantA, Tier: "security_3y", ActorID: subject2, ActorDisplay: "a"})
	taskID, out := scheduleAndApply(t, db, tenantA, subject2)
	assert.Equal(t, domain.RedactionMissed, out.Status)
	assert.EqualValues(t, 2, out.RowsRedacted, "hot rows (the actor row + the trigger row) are still redacted")
	assertMarker(t, readRow(t, db, hot.ID), taskID)
	task := readTask(t, db, taskID)
	assert.Equal(t, "missed", task.Status)
	require.NotNil(t, task.Rows)
	assert.EqualValues(t, 2, *task.Rows)
}

// D-1 / gap 34: the definer is owned by audit_reconciler and callable only
// by audit_app; audit_app still has no UPDATE on audit_events and INSERT
// only on the task table; audit_reconciler's UPDATE is column-scoped.
func TestRedaction_GrantsAndOwnership_D1(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	ctx := context.Background()
	_, err := db.raw.Exec(ctx, `CREATE ROLE stranger NOLOGIN`)
	require.NoError(t, err)
	exec := func(role string) bool {
		var ok bool
		require.NoError(t, db.raw.QueryRow(ctx, `SELECT has_function_privilege($1, 'apply_redaction(uuid)', 'EXECUTE')`, role).Scan(&ok))
		return ok
	}
	assert.True(t, exec("audit_app"))
	assert.False(t, exec("stranger"), "PUBLIC must not execute apply_redaction")

	var definer bool
	var owner string
	require.NoError(t, db.raw.QueryRow(ctx, `SELECT p.prosecdef, r.rolname FROM pg_proc p JOIN pg_roles r ON r.oid = p.proowner
		WHERE p.proname = 'apply_redaction'`).Scan(&definer, &owner))
	assert.True(t, definer, "SECURITY DEFINER")
	assert.Equal(t, "audit_reconciler", owner)

	assert.Equal(t, []string{"INSERT", "SELECT"}, tableGrants(t, db, "audit_app", "audit_events"), "AL-INV-1 unchanged")
	assert.Equal(t, []string{"INSERT"}, tableGrants(t, db, "audit_app", "audit_redaction_tasks"))

	row := seedRedactRow(t, db, redactRow{Tenant: tenantA, Tier: "security_3y", ActorID: uuid.NewString()})
	err = inTx(withTenant(ctx, tenantA), db.appPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE audit_events SET metadata = '{}' WHERE id = $1`, row.ID)
		return err
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "permission denied")

	err = inTx(withTenant(ctx, tenantA), db.appPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT count(*) FROM audit_redaction_tasks`)
		return err
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "permission denied", "audit_app has no SELECT on the task table")

	var cols []string
	rows, err := db.raw.Query(ctx, `SELECT column_name FROM information_schema.column_privileges
		WHERE grantee = 'audit_reconciler' AND table_name = 'audit_events' AND privilege_type = 'UPDATE' ORDER BY column_name`)
	require.NoError(t, err)
	for rows.Next() {
		var c string
		require.NoError(t, rows.Scan(&c))
		cols = append(cols, c)
	}
	rows.Close()
	assert.Equal(t, []string{"actor_display", "metadata"}, cols)

	err = inTx(ctx, db.reconPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE audit_events SET entry_type = 'x.y' WHERE id = $1`, row.ID)
		return err
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "permission denied", "reconciler UPDATE is column-scoped")

	_, err = pgadapter.NewAuditRepository(db.appPool).ApplyRedaction(ctx, uuid.NewString())
	require.Error(t, err, "unknown task id")
}

// redaction-retry's listing: pending tasks requested before the cutoff,
// oldest first, bounded by limit; finished tasks are excluded.
func TestRedaction_PendingTasks(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	ctx := context.Background()
	repo := pgadapter.NewRedactionRepository(db.reconPool)
	now := time.Now().UTC()
	insert := func(status string, age time.Duration) string {
		id := uuid.NewString()
		_, err := db.raw.Exec(ctx, `INSERT INTO audit_redaction_tasks
			(id, tenant_id, subject_actor_id, trigger_event_type, trigger_source_event_id, requested_at, status)
			VALUES ($1, $2, $3, 'UserDeleted', $4, $5, $6::audit_redaction_status)`,
			id, tenantA, uuid.NewString(), uuid.NewString(), now.Add(-age), status)
		require.NoError(t, err)
		return id
	}
	oldest := insert("pending", 3*time.Hour)
	older := insert("pending", 2*time.Hour)
	old := insert("pending", time.Hour)
	insert("pending", time.Minute) // too recent
	insert("applied", 4*time.Hour)
	insert("missed", 4*time.Hour)
	insert("not_applicable", 4*time.Hour)

	ids, err := repo.PendingTasks(ctx, now.Add(-30*time.Minute), 10)
	require.NoError(t, err)
	assert.Equal(t, []string{oldest, older, old}, ids)

	ids, err = repo.PendingTasks(ctx, now.Add(-30*time.Minute), 2)
	require.NoError(t, err)
	assert.Equal(t, []string{oldest, older}, ids)

	out, err := repo.ApplyRedaction(ctx, oldest)
	require.NoError(t, err, "the reconciler (owner) applies stuck tasks")
	assert.Equal(t, domain.RedactionNotApplicable, out.Status)
	ids, err = repo.PendingTasks(ctx, now.Add(-30*time.Minute), 10)
	require.NoError(t, err)
	assert.Equal(t, []string{older, old}, ids)
}
