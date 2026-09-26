//go:build integration

package postgres_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
)

// Phase 7 (LLD §8.5, §8.6, §15.4; AL-INV-9, AL-INV-12; D-19, D-20): the
// reconciler's archive repository and the audit_drop_partition() /
// audit_reopen_partition() definer functions (migration 000008).

// archiveMonth is the old month every archive test works on: 12 months
// back, created by audit_ensure_partitions' trailing window.
const archiveOffset = -12

func oldPartition(t *testing.T, db *testDB) (string, time.Time) {
	t.Helper()
	_, err := db.raw.Exec(context.Background(), `SELECT count(*) FROM audit_ensure_partitions(0, 13)`)
	require.NoError(t, err)
	return partitionName(archiveOffset), monthStart(archiveOffset)
}

// archRow inserts a row as superuser through the parent (routes by
// occurred_at), with an optional user target.
type archRow struct {
	Tenant     string
	At         time.Time
	Tier       string
	Actor      string // "" → a fresh uuid
	TargetUser string // "" → no target
	Metadata   string
}

func insertArchRow(t *testing.T, db *testDB, r archRow) string {
	t.Helper()
	id := uuid.NewString()
	if r.Actor == "" {
		r.Actor = uuid.NewString()
	}
	if r.Tier == "" {
		r.Tier = "security_3y"
	}
	if r.Metadata == "" {
		r.Metadata = `{"email":"x@example.com"}`
	}
	var tt, tid any
	if r.TargetUser != "" {
		tt, tid = "user", r.TargetUser
	}
	_, err := db.raw.Exec(context.Background(), `INSERT INTO audit_events (
    id, occurred_at, tenant_id, entry_type, action, actor_type, actor_id, target_type, target_id,
    source_service, source_event_type, source_event_id, retention_tier, ingest_mode, metadata)
VALUES ($1, $2, $3, 'user.updated', 'update', 'user', $4, $5, $6,
    'iam-user-profile', 'UserUpdated', $7, $8::audit_retention_tier, 'bus', $9::jsonb)`,
		id, r.At, r.Tenant, r.Actor, tt, tid, uuid.NewString(), r.Tier, r.Metadata)
	require.NoError(t, err)
	return id
}

func setArchiveState(t *testing.T, db *testDB, partition, tier, status string, month time.Time) {
	t.Helper()
	_, err := db.raw.Exec(context.Background(), `INSERT INTO audit_event_archive_state (partition_name, retention_tier, period_month, status)
VALUES ($1, $2::audit_retention_tier, $3, $4::audit_archive_status)
ON CONFLICT (partition_name, retention_tier) DO UPDATE SET status = EXCLUDED.status`, partition, tier, month, status)
	require.NoError(t, err)
}

func archiveStatus(t *testing.T, db *testDB, partition, tier string) string {
	t.Helper()
	var s string
	err := db.raw.QueryRow(context.Background(), `SELECT status::text FROM audit_event_archive_state
WHERE partition_name = $1 AND retention_tier = $2::audit_retention_tier`, partition, tier).Scan(&s)
	if err == pgx.ErrNoRows {
		return ""
	}
	require.NoError(t, err)
	return s
}

// seedObject inserts a manifest row directly (superuser).
func seedObject(t *testing.T, db *testDB, partition, tier, tenant string, month time.Time, part int, rows int64, sealed bool) string {
	t.Helper()
	key := domain.ArchiveKey(domain.RetentionTier(tier), tenant, month, part)
	_, err := db.raw.Exec(context.Background(), `INSERT INTO audit_archive_objects
    (partition_name, retention_tier, tenant_id, part, period_month, s3_bucket, s3_key, row_count, byte_size,
     min_occurred_at, max_occurred_at, min_id, max_id, sha256, sealed)
VALUES ($1, $2::audit_retention_tier, $3, $4, $5::date, 'iam-audit-archive', $6, $7, 1, $5::timestamptz, $5::timestamptz,
        '00000000-0000-0000-0000-000000000000', 'ffffffff-ffff-ffff-ffff-ffffffffffff', 'sha', $8)`,
		partition, tier, tenant, part, month, key, rows, sealed)
	require.NoError(t, err)
	return key
}

func partitionExists(t *testing.T, db *testDB, name string) bool {
	t.Helper()
	var ok bool
	require.NoError(t, db.raw.QueryRow(context.Background(), `SELECT to_regclass('public.' || $1) IS NOT NULL`, name).Scan(&ok))
	return ok
}

func partitionOfID(t *testing.T, db *testDB, id string) string {
	t.Helper()
	var p string
	require.NoError(t, db.raw.QueryRow(context.Background(),
		`SELECT tableoid::regclass::text FROM audit_events WHERE id = $1`, id).Scan(&p))
	return p
}

func insertTask(t *testing.T, db *testDB, tenant, subject, status string) string {
	t.Helper()
	id := uuid.NewString()
	_, err := db.raw.Exec(context.Background(), `INSERT INTO audit_redaction_tasks
    (id, tenant_id, subject_actor_id, trigger_event_type, trigger_source_event_id, status)
VALUES ($1, $2, $3, 'UserDeleted', $4, $5::audit_redaction_status)`, id, tenant, subject, uuid.NewString(), status)
	require.NoError(t, err)
	return id
}

// verifyBoth marks both retained tiers verified with manifest sums equal to
// the live counts, so the drop gate's only open question is the one a test
// is about.
func verifyBoth(t *testing.T, db *testDB, partition string, month time.Time) {
	t.Helper()
	ctx := context.Background()
	for _, tier := range []string{"security_3y", "compliance_7y"} {
		var n int64
		require.NoError(t, db.raw.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s WHERE retention_tier = $1::audit_retention_tier`,
			pgx.Identifier{partition}.Sanitize()), tier).Scan(&n))
		if n > 0 {
			seedObject(t, db, partition, tier, tenantA, month, 0, n, false)
		}
		setArchiveState(t, db, partition, tier, "verified", month)
	}
}

func TestArchiveRepo_PartitionsStatesTransitions(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	repo := pgadapter.NewArchiveRepository(db.reconPool)
	ctx := context.Background()
	p, month := oldPartition(t, db)

	parts, err := repo.Partitions(ctx)
	require.NoError(t, err)
	assert.Contains(t, parts, p)
	assert.Contains(t, parts, partitionName(0))
	assert.NotContains(t, parts, "audit_events_default")
	for i := 1; i < len(parts); i++ {
		assert.Less(t, parts[i-1], parts[i], "sorted")
	}

	st, err := repo.States(ctx, p)
	require.NoError(t, err)
	assert.Empty(t, st)

	require.NoError(t, repo.MarkArchiving(ctx, p, domain.TierSecurity3y, month))
	assert.Equal(t, "archiving", archiveStatus(t, db, p, "security_3y"))
	require.Error(t, repo.MarkVerified(ctx, p, domain.TierSecurity3y), "only an archived tier may become verified")

	require.NoError(t, repo.MarkFailed(ctx, p, domain.TierSecurity3y, "archive: boom"))
	var errText string
	require.NoError(t, db.raw.QueryRow(ctx, `SELECT error FROM audit_event_archive_state WHERE partition_name = $1 AND retention_tier = 'security_3y'`, p).Scan(&errText))
	assert.Equal(t, "failed", archiveStatus(t, db, p, "security_3y"))
	assert.Equal(t, "archive: boom", errText)

	require.NoError(t, repo.MarkArchiving(ctx, p, domain.TierSecurity3y, month)) // re-run clears the error
	require.NoError(t, db.raw.QueryRow(ctx, `SELECT coalesce(error, '') FROM audit_event_archive_state WHERE partition_name = $1 AND retention_tier = 'security_3y'`, p).Scan(&errText))
	assert.Empty(t, errText)
	require.NoError(t, repo.FinishArchive(ctx, p, domain.TierSecurity3y, nil, 0, "m", "security_3y/"))
	require.NoError(t, repo.MarkVerified(ctx, p, domain.TierSecurity3y))
	setArchiveState(t, db, p, "compliance_7y", "verified", month)

	st, err = repo.States(ctx, p)
	require.NoError(t, err)
	assert.Equal(t, "verified", st[domain.TierSecurity3y].Status)
	assert.Equal(t, "m", st[domain.TierSecurity3y].SHA256Manifest)

	require.NoError(t, repo.ResetForRearchive(ctx, p))
	assert.Equal(t, "pending", archiveStatus(t, db, p, "security_3y"))
	assert.Equal(t, "pending", archiveStatus(t, db, p, "compliance_7y"))

	// access_90d: recorded with its row count, never archived.
	for i := 0; i < 3; i++ {
		insertArchRow(t, db, archRow{Tenant: tenantA, At: month.Add(time.Hour), Tier: "access_90d"})
	}
	require.NoError(t, repo.MarkAccessExpiring(ctx, p, month))
	insertArchRow(t, db, archRow{Tenant: tenantB, At: month.Add(2 * time.Hour), Tier: "access_90d"})
	require.NoError(t, repo.MarkAccessExpiring(ctx, p, month)) // idempotent upsert refreshes the count
	var n int64
	require.NoError(t, db.raw.QueryRow(ctx, `SELECT row_count FROM audit_event_archive_state WHERE partition_name = $1 AND retention_tier = 'access_90d'`, p).Scan(&n))
	assert.EqualValues(t, 4, n)
	assert.Equal(t, "pending", archiveStatus(t, db, p, "access_90d"))

	require.Error(t, repo.MarkAccessExpiring(ctx, "audit_events_default", month), "only monthly names")
}

func TestArchiveRepo_PendingRedaction(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	repo := pgadapter.NewArchiveRepository(db.reconPool)
	ctx := context.Background()
	p, month := oldPartition(t, db)
	at := month.Add(time.Hour)

	actor, target, complianceOnly := uuid.NewString(), uuid.NewString(), uuid.NewString()
	insertArchRow(t, db, archRow{Tenant: tenantA, At: at, Actor: actor})
	insertArchRow(t, db, archRow{Tenant: tenantA, At: at, TargetUser: target})
	insertArchRow(t, db, archRow{Tenant: tenantA, At: at, Actor: complianceOnly, Tier: "compliance_7y"})

	check := func(want bool, msg string) {
		t.Helper()
		got, err := repo.PendingRedaction(ctx, p)
		require.NoError(t, err)
		assert.Equal(t, want, got, msg)
	}
	check(false, "no tasks")
	insertTask(t, db, tenantB, actor, "pending")
	check(false, "another tenant's task for the same id")
	insertTask(t, db, tenantA, complianceOnly, "pending")
	check(false, "subject appears only in compliance_7y rows")
	insertTask(t, db, tenantA, actor, "applied")
	check(false, "a finished task")
	id := insertTask(t, db, tenantA, target, "pending")
	check(true, "pending task for a user target")
	_, err := db.raw.Exec(ctx, `UPDATE audit_redaction_tasks SET status = 'applied' WHERE id = $1`, id)
	require.NoError(t, err)
	insertTask(t, db, tenantA, actor, "pending")
	check(true, "pending task for an actor")

	_, err = repo.PendingRedaction(ctx, "audit_events; DROP TABLE x")
	require.Error(t, err)
}

func TestArchiveRepo_ManifestLifecycle(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	repo := pgadapter.NewArchiveRepository(db.reconPool)
	ctx := context.Background()
	p, month := oldPartition(t, db)
	tier := domain.TierSecurity3y

	sealed0 := seedObject(t, db, p, "security_3y", tenantA, month, 0, 7, true)
	seedObject(t, db, p, "security_3y", tenantA, month, 1, 7, true)
	seedObject(t, db, p, "compliance_7y", tenantB, month, 4, 1, true) // other tier: ignored

	next, err := repo.NextParts(ctx, p, tier)
	require.NoError(t, err)
	assert.Equal(t, map[string]int{tenantA: 2}, next, "max sealed part + 1; tenants with no sealed parts absent")

	obj := func(tenant string, part int, rows int64) domain.ArchiveObject {
		return domain.ArchiveObject{
			Bucket: "iam-audit-archive", Key: domain.ArchiveKey(tier, tenant, month, part), TenantID: tenant, Tier: tier,
			PeriodMonth: month, Part: part, RowCount: rows, ByteSize: rows * 10,
			MinOccurredAt: month, MaxOccurredAt: month.Add(time.Hour),
			MinID: "01890000-0000-7000-8000-000000000000", MaxID: "01890000-0000-7000-8000-00000000ffff",
			SHA256: fmt.Sprintf("sha-%d", rows), SubjectIDs: []string{tenantA},
		}
	}
	a2, b0 := obj(tenantA, 2, 3), obj(tenantB, 0, 5)
	require.NoError(t, repo.MarkArchiving(ctx, p, tier, month))
	require.NoError(t, repo.UpsertObject(ctx, p, a2))
	require.NoError(t, repo.UpsertObject(ctx, p, b0))
	b0.RowCount, b0.SHA256 = 6, "sha-6"
	require.NoError(t, repo.UpsertObject(ctx, p, b0), "an unsealed row is rewritten (D-20)")
	nilSubjects := obj(tenantB, 1, 1)
	nilSubjects.SubjectIDs = nil
	require.NoError(t, repo.UpsertObject(ctx, p, nilSubjects), "nil subject_ids is stored as empty")

	// A sealed row is never overwritten, even by a same-key upsert.
	clash := obj(tenantA, 0, 999)
	require.NoError(t, repo.UpsertObject(ctx, p, clash))
	var rows int64
	var isSealed bool
	require.NoError(t, db.raw.QueryRow(ctx, `SELECT row_count, sealed FROM audit_archive_objects WHERE s3_key = $1`, sealed0).Scan(&rows, &isSealed))
	assert.EqualValues(t, 7, rows)
	assert.True(t, isSealed)

	uns, err := repo.UnsealedObjects(ctx, p, tier)
	require.NoError(t, err)
	require.Len(t, uns, 3)
	assert.Equal(t, a2.Key, uns[0].Key, "ordered by tenant, part")
	assert.Equal(t, "sha-6", uns[1].SHA256)
	assert.EqualValues(t, 6, uns[1].RowCount)
	assert.Equal(t, tier, uns[1].Tier)
	assert.True(t, uns[1].MaxOccurredAt.Equal(month.Add(time.Hour)))

	// FinishArchive: drop the stale unsealed row, keep the sealed ones.
	require.NoError(t, repo.FinishArchive(ctx, p, tier, []string{a2.Key, b0.Key}, 9, "manifest-sha", "security_3y/"))
	uns, err = repo.UnsealedObjects(ctx, p, tier)
	require.NoError(t, err)
	assert.Len(t, uns, 2)
	var sealedCount int
	require.NoError(t, db.raw.QueryRow(ctx, `SELECT count(*) FROM audit_archive_objects WHERE partition_name = $1 AND sealed`, p).Scan(&sealedCount))
	assert.Equal(t, 3, sealedCount)

	var status, sha, prefix, bucket string
	var rc int64
	var oc int
	var archivedAt *time.Time
	require.NoError(t, db.raw.QueryRow(ctx, `SELECT status::text, row_count, object_count, sha256_manifest, s3_prefix, s3_bucket, archived_at
FROM audit_event_archive_state WHERE partition_name = $1 AND retention_tier = 'security_3y'`, p).
		Scan(&status, &rc, &oc, &sha, &prefix, &bucket, &archivedAt))
	assert.Equal(t, "archived", status)
	assert.EqualValues(t, 9, rc)
	assert.Equal(t, 2, oc)
	assert.Equal(t, "manifest-sha", sha)
	assert.Equal(t, "security_3y/", prefix)
	assert.Equal(t, "iam-audit-archive", bucket)
	assert.NotNil(t, archivedAt)
}

func TestArchiveRepo_StreamTier(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	repo := pgadapter.NewArchiveRepository(db.reconPool)
	ctx := context.Background()
	p, month := oldPartition(t, db)

	b1 := insertArchRow(t, db, archRow{Tenant: tenantB, At: month.Add(time.Hour)})
	a2 := insertArchRow(t, db, archRow{Tenant: tenantA, At: month.Add(2 * time.Hour)})
	a1 := insertArchRow(t, db, archRow{Tenant: tenantA, At: month.Add(time.Hour)})
	insertArchRow(t, db, archRow{Tenant: tenantA, At: month.Add(time.Hour), Tier: "compliance_7y"})
	insertArchRow(t, db, archRow{Tenant: tenantA, At: monthStart(0).Add(time.Hour)}) // another month

	var got []string
	require.NoError(t, repo.StreamTier(ctx, p, domain.TierSecurity3y, func(e domain.AuditEntry) error {
		assert.Equal(t, domain.TierSecurity3y, e.RetentionTier)
		got = append(got, e.ID)
		return nil
	}))
	assert.Equal(t, []string{a1, a2, b1}, got, "tenant_id, occurred_at, id")

	boom := fmt.Errorf("stop")
	err := repo.StreamTier(ctx, p, domain.TierSecurity3y, func(domain.AuditEntry) error { return boom })
	assert.ErrorIs(t, err, boom)
	require.Error(t, repo.StreamTier(ctx, "audit_events_default", domain.TierSecurity3y, func(domain.AuditEntry) error { return nil }))
}

func TestArchiveRepo_PruneProcessedEvents(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	repo := pgadapter.NewArchiveRepository(db.reconPool)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_, err := db.raw.Exec(ctx, `INSERT INTO processed_events (event_id, consumer, processed_at) VALUES ($1, 'c', now() - interval '9 days')`, uuid.NewString())
		require.NoError(t, err)
	}
	_, err := db.raw.Exec(ctx, `INSERT INTO processed_events (event_id, consumer, processed_at) VALUES ($1, 'c', now() - interval '1 day')`, uuid.NewString())
	require.NoError(t, err)

	n, err := repo.PruneProcessedEvents(ctx, 8, 2)
	require.NoError(t, err)
	assert.EqualValues(t, 5, n, "deleted across batches of 2")
	var left int
	require.NoError(t, db.raw.QueryRow(ctx, `SELECT count(*) FROM processed_events`).Scan(&left))
	assert.Equal(t, 1, left)
}

func TestReconciler_DropBlockedWhenUnverified_ALINV9(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	repo := pgadapter.NewArchiveRepository(db.reconPool)
	ctx := context.Background()
	p, month := oldPartition(t, db)
	insertArchRow(t, db, archRow{Tenant: tenantA, At: month.Add(time.Hour)})

	res, err := repo.Drop(ctx, p)
	require.NoError(t, err)
	assert.Equal(t, domain.DropNotVerified, res)
	seedObject(t, db, p, "security_3y", tenantA, month, 0, 1, false) // security counts match
	setArchiveState(t, db, p, "security_3y", "verified", month)
	setArchiveState(t, db, p, "compliance_7y", "archived", month)
	res, err = repo.Drop(ctx, p)
	require.NoError(t, err)
	assert.Equal(t, domain.DropNotVerified, res, "every retained tier must be verified")
	assert.True(t, partitionExists(t, db, p), "AL-INV-9: never dropped unverified")
}

func TestReconciler_DropCountMismatch_ALD4(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	repo := pgadapter.NewArchiveRepository(db.reconPool)
	ctx := context.Background()
	p, month := oldPartition(t, db)
	insertArchRow(t, db, archRow{Tenant: tenantA, At: month.Add(time.Hour)})
	verifyBoth(t, db, p, month)
	insertArchRow(t, db, archRow{Tenant: tenantA, At: month.Add(2 * time.Hour)}) // late arrival after archival

	res, err := repo.Drop(ctx, p)
	require.NoError(t, err)
	assert.Equal(t, domain.DropCountMismatch, res)
	assert.True(t, partitionExists(t, db, p))

	// A sealed object of an earlier generation doesn't count toward the live rows.
	seedObject(t, db, p, "security_3y", tenantB, month, 9, 1, true)
	res, err = repo.Drop(ctx, p)
	require.NoError(t, err)
	assert.Equal(t, domain.DropCountMismatch, res)
}

func TestReconciler_RefusesPendingRedaction_ALINV12(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	repo := pgadapter.NewArchiveRepository(db.reconPool)
	ctx := context.Background()
	p, month := oldPartition(t, db)
	subject := uuid.NewString()
	insertArchRow(t, db, archRow{Tenant: tenantA, At: month.Add(time.Hour), Actor: subject})
	verifyBoth(t, db, p, month)
	insertTask(t, db, tenantA, subject, "pending")

	res, err := repo.Drop(ctx, p)
	require.NoError(t, err)
	assert.Equal(t, domain.DropRedactionPending, res)
	assert.True(t, partitionExists(t, db, p))
	var sealed int
	require.NoError(t, db.raw.QueryRow(ctx, `SELECT count(*) FROM audit_archive_objects WHERE partition_name = $1 AND sealed`, p).Scan(&sealed))
	assert.Zero(t, sealed, "nothing sealed on a refused drop")
}

func TestReconciler_DropSealsAndDrops_ALINV9(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	repo := pgadapter.NewArchiveRepository(db.reconPool)
	ctx := context.Background()
	p, month := oldPartition(t, db)
	insertArchRow(t, db, archRow{Tenant: tenantA, At: month.Add(time.Hour)})
	insertArchRow(t, db, archRow{Tenant: tenantA, At: month.Add(time.Hour), Tier: "access_90d"})
	other := insertArchRow(t, db, archRow{Tenant: tenantA, At: monthStart(0).Add(time.Hour)})
	verifyBoth(t, db, p, month) // compliance_7y has 0 rows = 0 manifest rows
	require.NoError(t, repo.MarkAccessExpiring(ctx, p, month))

	res, err := repo.Drop(ctx, p)
	require.NoError(t, err)
	require.Equal(t, domain.DropDropped, res)

	assert.False(t, partitionExists(t, db, p))
	assert.Equal(t, "dropped", archiveStatus(t, db, p, "security_3y"))
	assert.Equal(t, "dropped", archiveStatus(t, db, p, "compliance_7y"))
	assert.Equal(t, "expired", archiveStatus(t, db, p, "access_90d"))
	var unsealed int
	require.NoError(t, db.raw.QueryRow(ctx, `SELECT count(*) FROM audit_archive_objects WHERE partition_name = $1 AND NOT sealed`, p).Scan(&unsealed))
	assert.Zero(t, unsealed, "the manifest is sealed at drop (D-20)")
	assert.Equal(t, partitionName(0), partitionOfID(t, db, other), "other months untouched")

	res, err = repo.Drop(ctx, p)
	require.NoError(t, err)
	assert.Equal(t, domain.DropMissing, res)
	_, err = repo.Drop(ctx, "audit_events_default")
	require.Error(t, err, "only monthly partition names")
}

// D-19: a late row for a dropped month lands in DEFAULT; the reconciler
// re-opens the month, re-routes the row, and resets the retained tiers.
func TestReconciler_ReopenDroppedMonth_D19(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	repo := pgadapter.NewArchiveRepository(db.reconPool)
	ctx := context.Background()
	p, month := oldPartition(t, db)
	insertArchRow(t, db, archRow{Tenant: tenantA, At: month.Add(time.Hour)})
	verifyBoth(t, db, p, month)
	res, err := repo.Drop(ctx, p)
	require.NoError(t, err)
	require.Equal(t, domain.DropDropped, res)

	cands, err := repo.ReopenCandidates(ctx)
	require.NoError(t, err)
	assert.Empty(t, cands)

	// The late row arrives as audit_app, through the parent.
	late := newEvent(tenantA, month.Add(3*time.Hour))
	require.NoError(t, inTx(withTenant(ctx, tenantA), db.appPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, insertEventSQL, late.args()...)
		return err
	}))
	require.Equal(t, "audit_events_default", partitionOfID(t, db, late.ID))
	// A bad-clock row for a never-dropped month also sits in DEFAULT (RB-3).
	badClock := insertArchRow(t, db, archRow{Tenant: tenantA, At: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)})
	require.Equal(t, "audit_events_default", partitionOfID(t, db, badClock))

	cands, err = repo.ReopenCandidates(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{p}, cands, "only dropped months are re-open candidates")

	n, err := repo.Reopen(ctx, p)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
	assert.True(t, partitionExists(t, db, p))
	assert.Equal(t, p, partitionOfID(t, db, late.ID))
	assert.Equal(t, "audit_events_default", partitionOfID(t, db, badClock), "other months stay in DEFAULT")
	assert.Equal(t, "pending", archiveStatus(t, db, p, "security_3y"))
	assert.Equal(t, "pending", archiveStatus(t, db, p, "compliance_7y"))
	var sealed int
	require.NoError(t, db.raw.QueryRow(ctx, `SELECT count(*) FROM audit_archive_objects WHERE partition_name = $1 AND sealed`, p).Scan(&sealed))
	assert.Equal(t, 1, sealed, "the earlier generation's sealed objects are untouched")

	// The recreated partitions keep the parent's indexes and append-only
	// trigger, and audit_app still cannot UPDATE.
	for _, name := range []string{p, "audit_events_default"} {
		var idx int
		require.NoError(t, db.raw.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE tablename = $1`, name).Scan(&idx))
		assert.Greater(t, idx, 0, "%s indexes", name)
	}
	_, err = db.raw.Exec(ctx, `UPDATE audit_events SET action = 'x' WHERE id = $1`, late.ID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "append-only", "trigger cloned onto the re-opened partition")
	_, err = db.raw.Exec(ctx, `UPDATE audit_events SET action = 'x' WHERE id = $1`, badClock)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "append-only", "trigger cloned onto the new DEFAULT")
	err = inTx(withTenant(ctx, tenantA), db.appPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE audit_events SET action = 'x' WHERE id = $1`, late.ID)
		return err
	})
	require.Error(t, err)

	n, err = repo.Reopen(ctx, p)
	require.NoError(t, err)
	assert.Zero(t, n, "already open")
	_, err = repo.Reopen(ctx, "audit_events_2001_01")
	require.Error(t, err, "a never-dropped month is RB-3, not auto re-opened")
	_, err = repo.Reopen(ctx, "not_a_partition")
	require.Error(t, err)
	cands, err = repo.ReopenCandidates(ctx)
	require.NoError(t, err)
	assert.Empty(t, cands)
}

// 000008: a redaction or sweep that rewrites a row in an archived
// partition invalidates that partition's security_3y archive, so a stale
// object is never sealed; compliance is untouched.
func TestArchive_RedactionInvalidatesVerifiedArchive(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	redactions := pgadapter.NewRedactionRepository(db.reconPool)
	ctx := context.Background()
	p, month := oldPartition(t, db)
	subject := uuid.NewString()
	insertArchRow(t, db, archRow{Tenant: tenantA, At: month.Add(time.Hour), Actor: subject})
	setArchiveState(t, db, p, "security_3y", "verified", month)
	setArchiveState(t, db, p, "compliance_7y", "verified", month)
	setArchiveState(t, db, partitionName(0), "security_3y", "verified", monthStart(0)) // untouched partition

	task := insertTask(t, db, tenantA, subject, "pending")
	out, err := redactions.ApplyRedaction(ctx, task)
	require.NoError(t, err)
	assert.Equal(t, domain.RedactionApplied, out.Status)
	assert.Equal(t, "pending", archiveStatus(t, db, p, "security_3y"), "rewritten partition invalidated")
	assert.Equal(t, "verified", archiveStatus(t, db, p, "compliance_7y"))
	assert.Equal(t, "verified", archiveStatus(t, db, partitionName(0), "security_3y"))

	// The sweep invalidates too: a row that slipped past the ingest check.
	setArchiveState(t, db, p, "security_3y", "verified", month)
	insertArchRow(t, db, archRow{Tenant: tenantA, At: month.Add(2 * time.Hour), Actor: subject})
	n, err := redactions.Sweep(ctx, 90*24*time.Hour)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
	assert.Equal(t, "pending", archiveStatus(t, db, p, "security_3y"))

	// Nothing rewritten → nothing invalidated.
	setArchiveState(t, db, p, "security_3y", "verified", month)
	n, err = redactions.Sweep(ctx, 90*24*time.Hour)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Equal(t, "verified", archiveStatus(t, db, p, "security_3y"))
}

func TestArchive_PartitionFunctionGrants(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	ctx := context.Background()
	for _, fn := range []string{"audit_drop_partition(text)", "audit_reopen_partition(text)"} {
		var recon, app, public, definer bool
		require.NoError(t, db.raw.QueryRow(ctx, `SELECT has_function_privilege('audit_reconciler', $1, 'EXECUTE'),
       has_function_privilege('audit_app', $1, 'EXECUTE'),
       has_function_privilege('public', $1, 'EXECUTE'),
       (SELECT prosecdef FROM pg_proc WHERE oid = $1::regprocedure)`, fn).Scan(&recon, &app, &public, &definer))
		assert.True(t, recon, "%s: reconciler EXECUTE", fn)
		assert.False(t, app, "%s: audit_app must not EXECUTE", fn)
		assert.False(t, public, "%s: not PUBLIC", fn)
		assert.True(t, definer, "%s: SECURITY DEFINER", fn)
	}
	err := inTx(withTenant(ctx, tenantA), db.appPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT audit_drop_partition($1)`, partitionName(0))
		return err
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "permission denied")
	assert.True(t, partitionExists(t, db, partitionName(0)))
}
