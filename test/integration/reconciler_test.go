//go:build integration

package integration_test

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"sort"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/cmd/reconciler/jobs"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/outbound/postgres"
	s3adapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/outbound/s3"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/test/dbseed"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/test/fixtures"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/logger"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

// §14 "Reconciler" row, against floci S3 + PostgreSQL 15 with the real
// adapters (LLD §8.5, §8.6, §15.4; AL-INV-9, AL-INV-12, AL-D4; decisions
// D-10, D-17, D-19, D-20): archive → verify → drop, the drop refused while a
// redaction is pending or S3 fails, the late-arrival re-archive, and the
// automatic re-open of a dropped month.

const archiveBucket = "iam-audit-archive" // provisioned by scripts/init-floci.sh (Object Lock enabled)

// BUILD_PLAN gap 16 findings for floci 2.1.0-compat on the Object-Lock-
// enabled bucket (pinned by TestReconciler_FlociObjectLockSupport):
//   - versioning is Enabled and GetObject echoes ObjectLockMode /
//     ObjectLockRetainUntilDate, so these tests verify the lock on
//     read-back (Verify=true) as in AWS;
//   - a PutObject to a key whose current version is under COMPLIANCE
//     retention is refused (403 "Object is protected by COMPLIANCE
//     retention"), whereas real S3 stores it as a NEW version. The D-20
//     re-archive (rewrite a key as a new version) therefore cannot be run
//     under lock on floci; the late-arrival test writes without lock
//     headers.
var (
	flociEchoesObjectLock       = true
	flociRejectsLockedOverwrite = true
)

type recStack struct {
	seed      *dbseed.Pool
	reconPool *pgcommon.Pool
	appPool   *pgcommon.Pool
	s3        *awss3.Client
	store     *s3adapter.Store
	month     time.Time // the old month under test (now − 6 months, UTC)
	partition string
}

func s3Client() *awss3.Client {
	return awss3.NewFromConfig(shared.AWS, func(o *awss3.Options) { o.UsePathStyle = true })
}

func newRecStack(t *testing.T) *recStack {
	t.Helper()
	ctx := context.Background()
	roles := fixtures.CreateRoles(t, fixtures.StartPostgres(t))
	require.NoError(t, pgadapter.RunMigrations(ctx, roles.SuperDSN, nil))

	rcfg, _ := pgadapter.ReconcilerPoolConfig(roles.ReconcilerDSN, nil, nil)
	recon, err := pgcommon.NewPool(ctx, rcfg)
	require.NoError(t, err)
	t.Cleanup(recon.Close)
	acfg, _ := pgadapter.AppPoolConfig(roles.AppDSN, nil, nil)
	app, err := pgcommon.NewPool(ctx, acfg)
	require.NoError(t, err)
	t.Cleanup(app.Close)
	seed, err := dbseed.New(ctx, roles.SuperDSN)
	require.NoError(t, err)
	t.Cleanup(seed.Close)

	// Old monthly partitions: 12 trailing months, so month −6 exists.
	_, err = seed.Exec(ctx, `SELECT count(*) FROM audit_ensure_partitions(3, 12)`)
	require.NoError(t, err)

	now := time.Now().UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -6, 0)
	c := s3Client()
	return &recStack{
		seed: seed, reconPool: recon, appPool: app, s3: c,
		store:     s3adapter.New(c, archiveBucket, "").WithObjectLock(s3adapter.ObjectLock{Mode: "COMPLIANCE", Verify: flociEchoesObjectLock}),
		month:     month,
		partition: domain.PartitionName(month),
	}
}

// jctx is the reconciler job context wired as cmd/reconciler does, with a
// tiny part size so a tenant's tier splits into several objects.
func (s *recStack) jctx(t *testing.T, store port.ArchiveStore) *jobs.Context {
	t.Helper()
	archives := pgadapter.NewArchiveRepository(s.reconPool)
	log, err := logger.NewLogger("test")
	require.NoError(t, err)
	return &jobs.Context{
		Pool: s.reconPool, RawLogger: log, Logger: port.NewSlogStyleLogger(log),
		Partitions: pgadapter.NewPartitionRepository(s.reconPool),
		Archives:   archives, ArchiveStore: store, Ledger: archives,
		HotWindowDays: 90, PrecreateMonths: 3, WritableTrailingMonths: 3,
		ArchivePartMaxRows: 2, ArchiveWorkDir: t.TempDir(),
	}
}

type seedRow struct {
	ID, Tenant, Tier, Actor string
	TargetType, TargetID    *string
	At                      time.Time
}

// insert writes one row through the parent as superuser (routes to its
// month's partition, or DEFAULT if that month has none).
func (s *recStack) insert(t *testing.T, tenant, tier, actor string, target *string, at time.Time) seedRow {
	t.Helper()
	r := seedRow{ID: uuid.Must(uuid.NewV7()).String(), Tenant: tenant, Tier: tier, Actor: actor, At: at.UTC()}
	if target != nil {
		u := "user"
		r.TargetType, r.TargetID = &u, target
	}
	_, err := s.seed.Exec(context.Background(), `INSERT INTO audit_events
		(id, occurred_at, tenant_id, entry_type, action, actor_type, actor_id, actor_display, target_type, target_id,
		 source_service, source_topic, source_event_type, source_event_id, retention_tier, ingest_mode, metadata)
		VALUES ($1, $2, $3, 'user.updated', 'update', 'user', $4, 'someone@acme.example', $5, $6, 'seed', 'seed', 'Seed', $7,
		        $8::audit_retention_tier, 'bus', '{"k":"v"}')`,
		r.ID, r.At, tenant, actor, r.TargetType, r.TargetID, uuid.NewString(), tier)
	require.NoError(t, err)
	return r
}

// seedMonth writes, for each tenant, 3 security_3y (one with a user target),
// 1 compliance_7y and 1 access_90d row in the month under test.
func (s *recStack) seedMonth(t *testing.T, tenants ...string) map[string][]seedRow {
	t.Helper()
	out := map[string][]seedRow{}
	for _, tn := range tenants {
		base := s.month.AddDate(0, 0, 9)
		target := uuid.NewString()
		out[tn] = append(out[tn],
			s.insert(t, tn, "security_3y", uuid.NewString(), &target, base),
			s.insert(t, tn, "security_3y", uuid.NewString(), nil, base.Add(time.Hour)),
			s.insert(t, tn, "security_3y", uuid.NewString(), nil, base.Add(2*time.Hour)),
			s.insert(t, tn, "compliance_7y", uuid.NewString(), nil, base.Add(3*time.Hour)),
			s.insert(t, tn, "access_90d", uuid.NewString(), nil, base.Add(4*time.Hour)),
		)
	}
	return out
}

func (s *recStack) partitionExists(t *testing.T, name string) bool {
	t.Helper()
	var ok bool
	require.NoError(t, s.seed.QueryRow(context.Background(), `SELECT to_regclass('public.' || $1) IS NOT NULL`, name).Scan(&ok))
	return ok
}

func (s *recStack) states(t *testing.T) map[string]string {
	t.Helper()
	rows, err := s.seed.Query(context.Background(),
		`SELECT retention_tier::text, status::text FROM audit_event_archive_state WHERE partition_name = $1`, s.partition)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var tier, st string
		require.NoError(t, rows.Scan(&tier, &st))
		out[tier] = st
	}
	return out
}

type manifestRow struct {
	Key, SHA  string
	Part      int
	Rows      int64
	Sealed    bool
	Subjects  []string
	MinID     string
	MaxID     string
	TenantID  string
	TierLabel string
}

func (s *recStack) manifest(t *testing.T, tenant, tier string) []manifestRow {
	t.Helper()
	rows, err := s.seed.Query(context.Background(), `SELECT s3_key, sha256, part, row_count, sealed, subject_ids::text[],
		min_id::text, max_id::text, tenant_id::text, retention_tier::text
		FROM audit_archive_objects WHERE partition_name = $1 AND tenant_id = $2 AND retention_tier = $3::audit_retention_tier
		ORDER BY part`, s.partition, tenant, tier)
	require.NoError(t, err)
	defer rows.Close()
	var out []manifestRow
	for rows.Next() {
		var m manifestRow
		require.NoError(t, rows.Scan(&m.Key, &m.SHA, &m.Part, &m.Rows, &m.Sealed, &m.Subjects, &m.MinID, &m.MaxID, &m.TenantID, &m.TierLabel))
		out = append(out, m)
	}
	return out
}

// object GETs a key's current version, returning its body SHA-256 and the
// archive records it holds.
func (s *recStack) object(t *testing.T, key string) (string, []domain.ArchiveRecord) {
	t.Helper()
	out, err := s.s3.GetObject(context.Background(), &awss3.GetObjectInput{Bucket: aws.String(archiveBucket), Key: aws.String(key)})
	require.NoError(t, err, key)
	defer out.Body.Close()
	body, err := io.ReadAll(out.Body)
	require.NoError(t, err)
	sum := sha256.Sum256(body)
	zr, err := gzip.NewReader(bytes.NewReader(body))
	require.NoError(t, err)
	var recs []domain.ArchiveRecord
	sc := bufio.NewScanner(zr)
	for sc.Scan() {
		var r domain.ArchiveRecord
		require.NoError(t, json.Unmarshal(sc.Bytes(), &r))
		recs = append(recs, r)
	}
	require.NoError(t, sc.Err())
	return hex.EncodeToString(sum[:]), recs
}

func (s *recStack) query(t *testing.T) *service.QueryService {
	t.Helper()
	return service.NewQueryService(pgadapter.NewQueryRepository(s.appPool), s.store, service.QueryConfig{
		DefaultWindowDays: 3650, SyncMaxRows: 10000, SyncMaxBytes: 50 << 20,
	}, nil)
}

// al1 returns every entry id AL-1 serves for tenant over the month under
// test, walking the keyset to exhaustion.
func (s *recStack) al1(t *testing.T, tenant string) []string {
	t.Helper()
	from, to := s.month, s.month.AddDate(0, 1, 0).Add(-time.Nanosecond)
	q := s.query(t)
	var ids []string
	cursor := ""
	for {
		res, err := q.Query(context.Background(), service.QueryRequest{
			TenantID: tenant, Filter: domain.QueryFilter{From: &from, To: &to}, Cursor: cursor, Limit: 2,
		})
		require.NoError(t, err)
		require.Nil(t, res.Deferred)
		for _, e := range res.Events {
			ids = append(ids, e.ID)
		}
		if res.NextCursor == "" {
			return ids
		}
		cursor = res.NextCursor
	}
}

func idsOf(rows []seedRow, tiers ...string) []string {
	want := map[string]bool{}
	for _, t := range tiers {
		want[t] = true
	}
	var out []string
	for _, r := range rows {
		if want[r.Tier] {
			out = append(out, r.ID)
		}
	}
	sort.Strings(out)
	return out
}

func sorted(xs []string) []string {
	out := append([]string(nil), xs...)
	sort.Strings(out)
	return out
}

// BUILD_PLAN gap 16: what floci 2.1.0-compat does with Object Lock on the
// Object-Lock-enabled bucket, pinned so an emulator upgrade that changes it
// is noticed (see flociEchoesObjectLock / flociRejectsLockedOverwrite).
func TestReconciler_FlociObjectLockSupport(t *testing.T) {
	ctx := context.Background()
	c := s3Client()
	key := "probe/" + uuid.NewString()
	retain := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	put := func(body string) error {
		_, err := c.PutObject(ctx, &awss3.PutObjectInput{
			Bucket: aws.String(archiveBucket), Key: aws.String(key), Body: bytes.NewReader([]byte(body)),
			ObjectLockMode: s3types.ObjectLockModeCompliance, ObjectLockRetainUntilDate: aws.Time(retain),
		})
		return err
	}
	require.NoError(t, put("v1"), "PutObject with Object Lock headers is accepted")

	ver, err := c.GetBucketVersioning(ctx, &awss3.GetBucketVersioningInput{Bucket: aws.String(archiveBucket)})
	require.NoError(t, err)
	t.Logf("floci bucket versioning status: %q", ver.Status)

	out, err := c.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(archiveBucket), Key: aws.String(key)})
	require.NoError(t, err)
	_ = out.Body.Close()
	echoes := out.ObjectLockMode == s3types.ObjectLockModeCompliance && out.ObjectLockRetainUntilDate != nil
	t.Logf("floci GetObject echoes Object Lock: %v (mode=%q retain=%v)", echoes, out.ObjectLockMode, out.ObjectLockRetainUntilDate)
	assert.Equal(t, flociEchoesObjectLock, echoes, "update flociEchoesObjectLock to match the emulator")

	err = put("v2")
	t.Logf("floci second PutObject of a COMPLIANCE-locked key: err=%v", err)
	assert.Equal(t, flociRejectsLockedOverwrite, err != nil, "update flociRejectsLockedOverwrite to match the emulator")
}

// Happy path (§8.5): archive → verify → drop. Objects land at the D-10
// keys, split into parts, gunzip to exactly the tier's rows, and carry a
// manifest with a body checksum and subject set (D-17), sealed at drop
// (D-20). The partition is gone, recent ones are untouched, and the
// archived rows are then served from S3 by AL-1 and AL-2 (Phases 4/6).
func TestReconciler_ArchiveVerifyDrop_HappyPath(t *testing.T) {
	s := newRecStack(t)
	ctx := context.Background()
	t1, t2 := uuid.NewString(), uuid.NewString()
	rows := s.seedMonth(t, t1, t2)

	res, err := jobs.Reconcile(ctx, s.jctx(t, s.store))
	require.NoError(t, err)
	assert.Zero(t, res.Failed)

	assert.False(t, s.partitionExists(t, s.partition), "the archived month is dropped")
	now := time.Now().UTC()
	for i := 0; i <= 3; i++ {
		p := domain.PartitionName(time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -i, 0))
		assert.True(t, s.partitionExists(t, p), "writable month %s untouched", p)
	}
	assert.Equal(t, map[string]string{"security_3y": "dropped", "compliance_7y": "dropped", "access_90d": "expired"}, s.states(t))

	for _, tn := range []string{t1, t2} {
		sec := s.manifest(t, tn, "security_3y")
		require.Len(t, sec, 2, "3 rows at 2 per part → 2 parts")
		var got []string
		subjects := map[string]bool{}
		for i, m := range sec {
			assert.Equal(t, i, m.Part)
			assert.Equal(t, domain.ArchiveKey(domain.TierSecurity3y, tn, s.month, i), m.Key, "D-10 key")
			assert.True(t, m.Sealed, "sealed at drop (D-20)")
			sum, recs := s.object(t, m.Key)
			assert.Equal(t, m.SHA, sum, "manifest checksum is the object body's")
			assert.EqualValues(t, len(recs), m.Rows)
			for _, r := range recs {
				assert.Equal(t, tn, r.TenantID)
				assert.Equal(t, "security_3y", r.RetentionTier)
				got = append(got, r.ID)
			}
			for _, id := range m.Subjects {
				subjects[id] = true
			}
		}
		assert.Equal(t, idsOf(rows[tn], "security_3y"), sorted(got), "exactly the tier's rows")
		for _, r := range rows[tn] {
			if r.Tier != "security_3y" {
				continue
			}
			assert.True(t, subjects[r.Actor], "actor in subject_ids (D-17)")
			if r.TargetID != nil {
				assert.True(t, subjects[*r.TargetID], "user target in subject_ids (D-17)")
			}
		}

		comp := s.manifest(t, tn, "compliance_7y")
		require.Len(t, comp, 1)
		_, recs := s.object(t, comp[0].Key)
		require.Len(t, recs, 1)
		assert.Equal(t, idsOf(rows[tn], "compliance_7y"), []string{recs[0].ID})
		assert.Empty(t, s.manifest(t, tn, "access_90d"), "access_90d is never archived")

		// Closing the loop: the S3-only rows are served by AL-1 and AL-2.
		assert.Equal(t, idsOf(rows[tn], "security_3y", "compliance_7y"), sorted(s.al1(t, tn)),
			"AL-1 reads the archived month from S3; access_90d expired with the partition")
	}
	e, err := s.query(t).Get(ctx, t1, rows[t1][1].ID)
	require.NoError(t, err, "AL-2 finds an archived entry by id range (gap 32)")
	assert.Equal(t, rows[t1][1].ID, e.ID)
}

// AL-INV-12 backstop (§8.5): a pending redaction task whose subject has a
// security_3y row in the partition blocks its archival and drop.
func TestReconciler_BlockedByPendingRedaction_ALINV12(t *testing.T) {
	s := newRecStack(t)
	ctx := context.Background()
	tn, subject := uuid.NewString(), uuid.NewString()
	s.insert(t, tn, "security_3y", subject, nil, s.month.AddDate(0, 0, 3))
	_, err := s.seed.Exec(ctx, `INSERT INTO audit_redaction_tasks (tenant_id, subject_actor_id, trigger_event_type, trigger_source_event_id)
		VALUES ($1, $2, 'UserDeleted', $3)`, tn, subject, uuid.NewString())
	require.NoError(t, err)

	res, err := jobs.Reconcile(ctx, s.jctx(t, s.store))
	require.Error(t, err, "a blocked partition fails the run")
	assert.Equal(t, 1, res.Failed)
	assert.True(t, s.partitionExists(t, s.partition), "the partition is kept")
	assert.Empty(t, s.manifest(t, tn, "security_3y"), "nothing archived while redaction is pending")
}

// AL-INV-9: an S3 failure fails the tier and keeps the partition; nothing
// is sealed.
func TestReconciler_S3FailureNoDrop_ALINV9(t *testing.T) {
	s := newRecStack(t)
	ctx := context.Background()
	tn := uuid.NewString()
	s.seedMonth(t, tn)
	broken := s3adapter.New(s.s3, "no-such-bucket-"+uuid.NewString()[:8], "").
		WithObjectLock(s3adapter.ObjectLock{Mode: "COMPLIANCE"})

	res, err := jobs.Reconcile(ctx, s.jctx(t, broken))
	require.Error(t, err)
	assert.GreaterOrEqual(t, res.Failed, 1)
	assert.True(t, s.partitionExists(t, s.partition), "no drop without verified archives")
	st := s.states(t)
	assert.Equal(t, "failed", st["security_3y"])
	assert.Equal(t, "failed", st["compliance_7y"])
	var sealed int
	require.NoError(t, s.seed.QueryRow(ctx, `SELECT count(*) FROM audit_archive_objects WHERE sealed`).Scan(&sealed))
	assert.Zero(t, sealed)
}

// lateStore inserts a late row once, during the first archive upload —
// after the tier's snapshot was taken, before the drop gate runs.
type lateStore struct {
	port.ArchiveStore
	hook func()
}

func (l *lateStore) PutArchive(ctx context.Context, key string, body io.ReadSeeker, size int64, retainUntil time.Time) error {
	if l.hook != nil {
		h := l.hook
		l.hook = nil
		h()
	}
	return l.ArchiveStore.PutArchive(ctx, key, body, size, retainUntil)
}

// AL-D4 (D-19, not-yet-dropped case): a row that lands after archival makes
// the drop gate report count_mismatch; the month is re-archived in the same
// run (whole tier rewritten as new object versions, D-20) and only then
// dropped. The tenant's current objects contain the late row.
func TestReconciler_LateArrivalNotDropped_ALD4(t *testing.T) {
	s := newRecStack(t)
	ctx := context.Background()
	tn := uuid.NewString()
	rows := s.seedMonth(t, tn)
	var late seedRow
	// Unlocked: the re-archive rewrites keys as new versions, which floci
	// refuses under COMPLIANCE retention (gap 16) but real S3 accepts.
	unlocked := s3adapter.New(s.s3, archiveBucket, "")
	store := &lateStore{ArchiveStore: unlocked, hook: func() {
		late = s.insert(t, tn, "security_3y", uuid.NewString(), nil, s.month.AddDate(0, 0, 20))
	}}

	_, err := jobs.Reconcile(ctx, s.jctx(t, store))
	require.NoError(t, err)
	require.NotEmpty(t, late.ID, "the late row was inserted mid-archive")
	assert.False(t, s.partitionExists(t, s.partition), "dropped after the re-archive")

	var got []string
	for _, m := range s.manifest(t, tn, "security_3y") {
		assert.True(t, m.Sealed)
		_, recs := s.object(t, m.Key)
		for _, r := range recs {
			got = append(got, r.ID)
		}
	}
	want := append(idsOf(rows[tn], "security_3y"), late.ID)
	assert.Equal(t, sorted(want), sorted(got), "current object versions hold every row, including the late one")
	assert.Equal(t, sorted(append(idsOf(rows[tn], "security_3y", "compliance_7y"), late.ID)), sorted(s.al1(t, tn)))
}

// D-19 (dropped case): a late row for an already-dropped month lands in
// DEFAULT; the next run re-opens the month, archives it as new parts after
// the sealed ones, and drops it again. AL-1 then serves the original and the
// late rows from S3 with no duplicates.
func TestReconciler_ReopenDroppedMonth_D19(t *testing.T) {
	s := newRecStack(t)
	ctx := context.Background()
	tn := uuid.NewString()
	rows := s.seedMonth(t, tn)
	_, err := jobs.Reconcile(ctx, s.jctx(t, s.store))
	require.NoError(t, err)
	require.False(t, s.partitionExists(t, s.partition))
	before := s.manifest(t, tn, "security_3y")
	require.Len(t, before, 2)

	late := s.insert(t, tn, "security_3y", uuid.NewString(), nil, s.month.AddDate(0, 0, 25))
	var inDefault int
	require.NoError(t, s.seed.QueryRow(ctx, `SELECT count(*) FROM audit_events_default`).Scan(&inDefault))
	require.Equal(t, 1, inDefault, "the late row lands in DEFAULT (its month is dropped)")

	_, err = jobs.Reconcile(ctx, s.jctx(t, s.store))
	require.NoError(t, err)
	require.NoError(t, s.seed.QueryRow(ctx, `SELECT count(*) FROM audit_events_default`).Scan(&inDefault))
	assert.Zero(t, inDefault, "DEFAULT is empty again")
	assert.False(t, s.partitionExists(t, s.partition), "re-opened, re-archived and dropped again")
	assert.Equal(t, "dropped", s.states(t)["security_3y"])

	after := s.manifest(t, tn, "security_3y")
	require.Len(t, after, 3, "one new part appended after the two sealed ones")
	for i := range before {
		assert.Equal(t, before[i].Key, after[i].Key)
		assert.Equal(t, before[i].SHA, after[i].SHA, "sealed parts are never rewritten (D-20)")
	}
	assert.Equal(t, 2, after[2].Part)
	assert.True(t, after[2].Sealed)
	_, recs := s.object(t, after[2].Key)
	require.Len(t, recs, 1)
	assert.Equal(t, late.ID, recs[0].ID)

	got := s.al1(t, tn)
	assert.Equal(t, sorted(append(idsOf(rows[tn], "security_3y", "compliance_7y"), late.ID)), sorted(got),
		"original + late rows from S3, no duplicates")
}
