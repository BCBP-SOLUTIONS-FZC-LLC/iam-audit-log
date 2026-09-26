//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
)

// queryRow is an audit_events row with every AL-1 filter column settable.
type queryRow struct {
	ID, Tenant, EntryType, ActorType, Source, Tier string
	ActorID, TargetType, TargetID                  any // nil → NULL
	At                                             time.Time
}

func newQueryRow(tenant string, at time.Time) queryRow {
	return queryRow{
		ID: uuid.NewString(), Tenant: tenant, EntryType: "user.updated", ActorType: "user",
		ActorID: uuid.NewString(), Source: "iam-user-profile", Tier: "security_3y", At: at.UTC().Truncate(time.Microsecond),
	}
}

const insertQueryRowSQL = `INSERT INTO audit_events (
    id, occurred_at, tenant_id, entry_type, action, actor_type, actor_id, target_type, target_id,
    source_service, source_event_type, source_event_id, retention_tier, ingest_mode, metadata)
VALUES ($1, $2, $3, $4, 'update', $5::audit_actor_type, $6, $7, $8,
    $9, 'UserUpdated', $10, $11::audit_retention_tier, 'bus', '{"k":"v"}')`

// seedRows inserts rows as superuser (bypasses RLS).
func seedRows(t *testing.T, db *testDB, rows ...queryRow) {
	t.Helper()
	for _, r := range rows {
		_, err := db.raw.Exec(context.Background(), insertQueryRowSQL, r.ID, r.At, r.Tenant, r.EntryType, r.ActorType,
			r.ActorID, r.TargetType, r.TargetID, r.Source, uuid.NewString(), r.Tier)
		require.NoError(t, err)
	}
}

func ids(es []domain.AuditEntry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.ID
	}
	return out
}

// wide is a [from, to] range covering every seeded row.
func wide() (time.Time, time.Time) {
	return monthStart(-2), time.Now().UTC().Add(time.Hour)
}

// §5.1: keyset order is (occurred_at DESC, id DESC); ties on occurred_at
// are broken by id, and a cursor continues exactly after the last row.
func TestQuery_KeysetOrderAndTies(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	repo := pgadapter.NewQueryRepository(db.appPool)
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)

	var rows []queryRow
	for i := 0; i < 3; i++ { // three rows sharing one occurred_at
		rows = append(rows, newQueryRow(tenantA, base))
	}
	rows = append(rows, newQueryRow(tenantA, base.Add(time.Minute)), newQueryRow(tenantA, base.Add(-time.Minute)))
	seedRows(t, db, rows...)

	from, to := wide()
	all, err := repo.Page(ctx, tenantA, domain.QueryFilter{}, from, to, nil, 100)
	require.NoError(t, err)
	require.Len(t, all, 5)
	for i := 1; i < len(all); i++ {
		assert.True(t, domain.NewerFirst(all[i-1], all[i]), "row %d must sort before row %d", i-1, i)
	}
	assert.Equal(t, rows[3].ID, all[0].ID, "newest first")
	assert.Equal(t, rows[4].ID, all[4].ID, "oldest last")

	// Page size 2 walks the tie group without dupes or gaps.
	var walked []string
	var cur *domain.Cursor
	for {
		page, err := repo.Page(ctx, tenantA, domain.QueryFilter{}, from, to, cur, 2)
		require.NoError(t, err)
		walked = append(walked, ids(page)...)
		if len(page) < 2 {
			break
		}
		last := page[len(page)-1]
		cur = &domain.Cursor{OccurredAt: last.OccurredAt, ID: last.ID}
	}
	assert.Equal(t, ids(all), walked)

	e := all[0]
	assert.Equal(t, tenantA, e.TenantID)
	assert.Equal(t, "user.updated", e.EntryType)
	assert.Equal(t, domain.ActorUser, e.Actor.Type)
	assert.JSONEq(t, `{"k":"v"}`, string(e.Metadata))
}

// LLD §14 "Query": keyset pagination is stable under concurrent ingestion —
// rows ingested between page reads (newer than the cursor) never shift,
// duplicate or drop rows on later pages.
func TestQuery_KeysetStableUnderConcurrentIngestion(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	repo := pgadapter.NewQueryRepository(db.appPool)
	ctx := context.Background()
	base := time.Now().UTC().Add(-2 * time.Hour)

	var want []string
	for i := 0; i < 10; i++ {
		r := newQueryRow(tenantA, base.Add(time.Duration(i)*time.Second))
		seedRows(t, db, r)
		want = append([]string{r.ID}, want...) // newest first
	}
	from, to := wide()
	var got []string
	var cur *domain.Cursor
	for page := 0; ; page++ {
		rows, err := repo.Page(ctx, tenantA, domain.QueryFilter{}, from, to, cur, 3)
		require.NoError(t, err)
		got = append(got, ids(rows)...)
		// Concurrent ingestion lands newer than every cursor position.
		seedRows(t, db, newQueryRow(tenantA, time.Now().UTC().Add(-time.Minute)))
		if len(rows) < 3 {
			break
		}
		last := rows[len(rows)-1]
		cur = &domain.Cursor{OccurredAt: last.OccurredAt, ID: last.ID}
		require.Less(t, page, 10)
	}
	assert.Equal(t, want, got, "exactly the original rows, in order, once each")
}

// §5.4 AL-1: every filter narrows in SQL; [from, to] is inclusive.
func TestQuery_Filters(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	repo := pgadapter.NewQueryRepository(db.appPool)
	ctx := context.Background()
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)

	plain := newQueryRow(tenantA, at)
	granted := newQueryRow(tenantA, at.Add(time.Second))
	granted.EntryType, granted.Source, granted.Tier = "membership.department.granted", "iam-org-membership", "compliance_7y"
	granted.TargetType, granted.TargetID = "department", "dept-1"
	otherTarget := newQueryRow(tenantA, at.Add(2*time.Second))
	otherTarget.TargetType, otherTarget.TargetID = "department", "dept-2"
	system := newQueryRow(tenantA, at.Add(3*time.Second))
	system.ActorType, system.ActorID = "iam_system", domain.IAMSystemActorID
	anon := newQueryRow(tenantA, at.Add(4*time.Second))
	anon.ActorType, anon.ActorID, anon.Tier = "anonymous", nil, "access_90d"
	seedRows(t, db, plain, granted, otherTarget, system, anon)

	from, to := wide()
	cases := []struct {
		name string
		f    domain.QueryFilter
		want []string
	}{
		{"entry_type list", domain.QueryFilter{EntryTypes: []string{"membership.department.granted", "nope.none"}}, []string{granted.ID}},
		{"entry_type multi", domain.QueryFilter{EntryTypes: []string{"membership.department.granted", "user.updated"}},
			[]string{anon.ID, system.ID, otherTarget.ID, granted.ID, plain.ID}},
		{"actor_id", domain.QueryFilter{ActorID: plain.ActorID.(string)}, []string{plain.ID}},
		{"actor_type", domain.QueryFilter{ActorType: "iam_system"}, []string{system.ID}},
		{"actor_type anonymous", domain.QueryFilter{ActorType: "anonymous"}, []string{anon.ID}},
		{"target_type", domain.QueryFilter{TargetType: "department"}, []string{otherTarget.ID, granted.ID}},
		{"target_type+id", domain.QueryFilter{TargetType: "department", TargetID: "dept-1"}, []string{granted.ID}},
		{"source_service", domain.QueryFilter{SourceService: "iam-org-membership"}, []string{granted.ID}},
		{"retention_tier", domain.QueryFilter{RetentionTier: "access_90d"}, []string{anon.ID}},
		{"combined miss", domain.QueryFilter{ActorType: "anonymous", RetentionTier: "compliance_7y"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := repo.Page(ctx, tenantA, tc.f, from, to, nil, 100)
			require.NoError(t, err)
			assert.Equal(t, tc.want, nilIfEmpty(ids(got)))
		})
	}

	t.Run("inclusive bounds", func(t *testing.T) {
		got, err := repo.Page(ctx, tenantA, domain.QueryFilter{}, granted.At, system.At, nil, 100)
		require.NoError(t, err)
		assert.Equal(t, []string{system.ID, otherTarget.ID, granted.ID}, ids(got))
	})
	t.Run("empty range", func(t *testing.T) {
		got, err := repo.Page(ctx, tenantA, domain.QueryFilter{}, at.Add(-time.Hour), at.Add(-time.Minute), nil, 100)
		require.NoError(t, err)
		assert.Empty(t, got)
	})
	t.Run("target round-trips", func(t *testing.T) {
		e, err := repo.Get(ctx, tenantA, granted.ID)
		require.NoError(t, err)
		require.NotNil(t, e.Target)
		assert.Equal(t, domain.TargetRef{Type: "department", ID: "dept-1"}, *e.Target)
		assert.Equal(t, domain.TierCompliance7y, e.RetentionTier)
		a, err := repo.Get(ctx, tenantA, anon.ID)
		require.NoError(t, err)
		assert.Nil(t, a.Target)
		assert.Empty(t, a.Actor.ID)
	})
}

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

// AL-INV-3 on the read path: tenant A's reader never sees tenant B's rows,
// plan window, archive manifest or export jobs. On the RLS tables the
// tenant_id predicate is only a planner hint — RLS is the control (proved
// by querying with A's GUC for B's tenant id). tenant_plan_window is
// RLS-exempt by design (§4.6), so there the reader's predicate isolates.
func TestQuery_TenantIsolation_ALINV3(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	repo := pgadapter.NewQueryRepository(db.appPool)
	exports := pgadapter.NewExportRepository(db.appPool)
	ctx := context.Background()

	b := newQueryRow(tenantB, time.Now().UTC().Add(-time.Hour))
	seedRows(t, db, b)
	_, err := db.raw.Exec(ctx, `INSERT INTO tenant_plan_window (tenant_id, plan_code, query_window_days, last_event_at) VALUES ($1, 'pro', 1095, now())`, tenantB)
	require.NoError(t, err)
	seedArchiveObject(t, db, archiveSeed{Tenant: tenantB, Partition: "audit_events_2019_01", Tier: "security_3y",
		Min: time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC), Max: time.Date(2019, 1, 31, 0, 0, 0, 0, time.UTC)})
	job, err := exports.Create(ctx, domain.ExportJob{ID: uuid.NewString(), TenantID: tenantB, RequestedBy: uuid.NewString()})
	require.NoError(t, err)

	from, to := time.Date(2018, 1, 1, 0, 0, 0, 0, time.UTC), time.Now().UTC().Add(time.Hour)

	// Tenant B sees its own data.
	page, err := repo.Page(ctx, tenantB, domain.QueryFilter{}, from, to, nil, 10)
	require.NoError(t, err)
	assert.Len(t, page, 1)

	// Tenant A sees nothing of B's.
	page, err = repo.Page(ctx, tenantA, domain.QueryFilter{}, from, to, nil, 10)
	require.NoError(t, err)
	assert.Empty(t, page)
	_, err = repo.Get(ctx, tenantA, b.ID)
	assert.ErrorIs(t, err, port.ErrNotFound)
	pw, err := repo.PlanWindow(ctx, tenantA)
	require.NoError(t, err)
	assert.Nil(t, pw)
	objs, err := repo.ArchivedObjects(ctx, tenantA, from, to, "")
	require.NoError(t, err)
	assert.Empty(t, objs)
	_, err = exports.Get(ctx, tenantA, job.ID)
	assert.ErrorIs(t, err, port.ErrNotFound)

	// Even naming B's tenant in the predicate under A's GUC yields nothing:
	// RLS, not the WHERE clause, isolates.
	var n int
	require.NoError(t, inTx(withTenant(ctx, tenantA), db.appPool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM audit_events WHERE tenant_id = $1) +
			(SELECT count(*) FROM audit_archive_objects WHERE tenant_id = $1) +
			(SELECT count(*) FROM audit_export_jobs WHERE tenant_id = $1)`, tenantB).Scan(&n)
	}))
	assert.Zero(t, n)
}

// AL-2 lookups: absent id → port.ErrNotFound; no plan row → nil.
func TestQuery_GetNotFoundAndPlanWindow(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	repo := pgadapter.NewQueryRepository(db.appPool)
	ctx := context.Background()

	_, err := repo.Get(ctx, tenantA, uuid.NewString())
	assert.ErrorIs(t, err, port.ErrNotFound)

	pw, err := repo.PlanWindow(ctx, tenantA)
	require.NoError(t, err)
	assert.Nil(t, pw)

	_, err = db.raw.Exec(ctx, `INSERT INTO tenant_plan_window (tenant_id, plan_code, query_window_days, last_event_at) VALUES ($1, 'enterprise', 2555, now())`, tenantA)
	require.NoError(t, err)
	pw, err = repo.PlanWindow(ctx, tenantA)
	require.NoError(t, err)
	require.NotNil(t, pw)
	assert.Equal(t, domain.PlanWindowRow{PlanCode: "enterprise", QueryWindowDays: 2555}, *pw)

	r := newQueryRow(tenantA, time.Now().UTC().Add(-time.Minute))
	seedRows(t, db, r)
	e, err := repo.Get(ctx, tenantA, r.ID)
	require.NoError(t, err)
	assert.Equal(t, r.ID, e.ID)
	assert.True(t, r.At.Equal(e.OccurredAt))
}

type archiveSeed struct {
	Tenant, Partition, Tier string
	Part                    int
	Min, Max                time.Time
	Rows, Bytes             int64
	MinID, MaxID            string // "" → the full uuid range
}

// seedArchiveObject inserts a manifest row as superuser (audit_app has
// SELECT only; the reconciler writes these in Phase 7).
func seedArchiveObject(t *testing.T, db *testDB, s archiveSeed) string {
	t.Helper()
	month := time.Date(s.Min.Year(), s.Min.Month(), 1, 0, 0, 0, 0, time.UTC)
	key := domain.ArchiveKey(domain.RetentionTier(s.Tier), s.Tenant, month, s.Part)
	if s.MinID == "" {
		s.MinID = "00000000-0000-0000-0000-000000000000"
	}
	if s.MaxID == "" {
		s.MaxID = "ffffffff-ffff-ffff-ffff-ffffffffffff"
	}
	_, err := db.raw.Exec(context.Background(), `INSERT INTO audit_archive_objects
		(partition_name, retention_tier, tenant_id, part, period_month, s3_bucket, s3_key,
		 row_count, byte_size, min_occurred_at, max_occurred_at, min_id, max_id, sha256)
		VALUES ($1, $2::audit_retention_tier, $3, $4, $5, 'iam-audit-archive', $6, $7, $8, $9, $10, $11, $12, 'deadbeef')`,
		s.Partition, s.Tier, s.Tenant, s.Part, month, key, s.Rows, s.Bytes, s.Min, s.Max, s.MinID, s.MaxID)
	require.NoError(t, err)
	return key
}

// Decision D-12: partition existence is the hot/archived boundary. A
// manifest row whose partition still exists is excluded (its rows are read
// from RDS); a dropped month's objects are returned, overlap-filtered,
// tier-filtered, newest first.
func TestQuery_ArchivedObjectsOnlyForDroppedPartitions_D12(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	repo := pgadapter.NewQueryRepository(db.appPool)
	ctx := context.Background()
	d := func(y int, m time.Month, day int) time.Time { return time.Date(y, m, day, 0, 0, 0, 0, time.UTC) }

	// Hot: the current month's partition exists.
	seedArchiveObject(t, db, archiveSeed{Tenant: tenantA, Partition: partitionName(0), Tier: "security_3y",
		Min: monthStart(0), Max: monthStart(0).Add(time.Hour), Rows: 5, Bytes: 500})
	jan0 := seedArchiveObject(t, db, archiveSeed{Tenant: tenantA, Partition: "audit_events_2019_01", Tier: "security_3y",
		Min: d(2019, 1, 1), Max: d(2019, 1, 20), Rows: 10, Bytes: 1000})
	jan1 := seedArchiveObject(t, db, archiveSeed{Tenant: tenantA, Partition: "audit_events_2019_01", Tier: "security_3y", Part: 1,
		Min: d(2019, 1, 20), Max: d(2019, 1, 31), Rows: 10, Bytes: 1000})
	janC := seedArchiveObject(t, db, archiveSeed{Tenant: tenantA, Partition: "audit_events_2019_01", Tier: "compliance_7y",
		Min: d(2019, 1, 2), Max: d(2019, 1, 25), Rows: 3, Bytes: 300})
	feb := seedArchiveObject(t, db, archiveSeed{Tenant: tenantA, Partition: "audit_events_2019_02", Tier: "security_3y",
		Min: d(2019, 2, 1), Max: d(2019, 2, 28), Rows: 7, Bytes: 700})

	keys := func(os []domain.ArchiveObject) []string {
		out := make([]string, len(os))
		for i, o := range os {
			out[i] = o.Key
		}
		return out
	}

	all, err := repo.ArchivedObjects(ctx, tenantA, d(2018, 1, 1), time.Now().UTC().Add(time.Hour), "")
	require.NoError(t, err)
	assert.Equal(t, []string{feb, jan1, janC, jan0}, keys(all), "existing partition excluded; newest max_occurred_at first")
	o := all[0]
	assert.Equal(t, "iam-audit-archive", o.Bucket)
	assert.Equal(t, tenantA, o.TenantID)
	assert.Equal(t, domain.TierSecurity3y, o.Tier)
	assert.Equal(t, int64(7), o.RowCount)
	assert.Equal(t, int64(700), o.ByteSize)
	assert.True(t, o.PeriodMonth.Equal(d(2019, 2, 1)))

	// Overlap: [Jan 21, Jan 22] touches jan1 and janC only; boundaries inclusive.
	got, err := repo.ArchivedObjects(ctx, tenantA, d(2019, 1, 21), d(2019, 1, 22), "")
	require.NoError(t, err)
	assert.Equal(t, []string{jan1, janC}, keys(got))
	got, err = repo.ArchivedObjects(ctx, tenantA, d(2019, 1, 31), d(2019, 2, 1), "")
	require.NoError(t, err)
	assert.Equal(t, []string{feb, jan1}, keys(got), "min/max bounds are inclusive")

	// Tier filter.
	got, err = repo.ArchivedObjects(ctx, tenantA, d(2018, 1, 1), d(2020, 1, 1), "compliance_7y")
	require.NoError(t, err)
	assert.Equal(t, []string{janC}, keys(got))
}

// Gap 32: the AL-2 archived fallback reads only dropped-partition objects
// whose [min_id, max_id] holds the id (bounds inclusive) and that reach the
// window start; the tenant's GUC scopes it like every other read.
func TestQuery_ArchivedObjectsForID_Gap32(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	repo := pgadapter.NewQueryRepository(db.appPool)
	ctx := context.Background()
	d := func(y int, m time.Month, day int) time.Time { return time.Date(y, m, day, 0, 0, 0, 0, time.UTC) }
	lo, mid, hi := "01890000-0000-7000-8000-000000000000", "01890000-0000-7000-8000-000000000500", "01890000-0000-7000-8000-000000000fff"
	other := "01990000-0000-7000-8000-000000000000"

	hit := seedArchiveObject(t, db, archiveSeed{Tenant: tenantA, Partition: "audit_events_2019_01", Tier: "security_3y",
		Min: d(2019, 1, 1), Max: d(2019, 1, 31), Rows: 1, Bytes: 1, MinID: lo, MaxID: hi})
	seedArchiveObject(t, db, archiveSeed{Tenant: tenantA, Partition: "audit_events_2019_02", Tier: "security_3y",
		Min: d(2019, 2, 1), Max: d(2019, 2, 28), Rows: 1, Bytes: 1, MinID: other, MaxID: other})
	seedArchiveObject(t, db, archiveSeed{Tenant: tenantA, Partition: partitionName(0), Tier: "security_3y", // hot
		Min: monthStart(0), Max: monthStart(0).Add(time.Hour), Rows: 1, Bytes: 1, MinID: lo, MaxID: hi})
	seedArchiveObject(t, db, archiveSeed{Tenant: tenantB, Partition: "audit_events_2019_01", Tier: "security_3y",
		Min: d(2019, 1, 1), Max: d(2019, 1, 31), Rows: 1, Bytes: 1, MinID: lo, MaxID: hi})

	for _, id := range []string{lo, mid, hi} {
		got, err := repo.ArchivedObjectsForID(ctx, tenantA, id, d(2018, 1, 1))
		require.NoError(t, err)
		require.Len(t, got, 1, "id %s", id)
		assert.Equal(t, hit, got[0].Key)
		assert.Equal(t, lo, got[0].MinID)
		assert.Equal(t, hi, got[0].MaxID)
	}
	got, err := repo.ArchivedObjectsForID(ctx, tenantA, "01880000-0000-7000-8000-000000000000", d(2018, 1, 1))
	require.NoError(t, err)
	assert.Empty(t, got, "id below every range")
	got, err = repo.ArchivedObjectsForID(ctx, tenantA, mid, d(2019, 2, 1))
	require.NoError(t, err)
	assert.Empty(t, got, "object entirely before the window start")
}

// D-10 manifest table: RLS-protected like audit_events; audit_app reads
// only (the reconciler writes), and cannot forge a manifest row.
func TestArchiveObjects_GrantsAndRLS(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	ctx := context.Background()
	assert.Equal(t, []string{"SELECT"}, tableGrants(t, db, "audit_app", "audit_archive_objects"))
	assert.Equal(t, []string{"INSERT", "SELECT"}, tableGrants(t, db, "audit_reconciler", "audit_archive_objects"))

	insert := `INSERT INTO audit_archive_objects (partition_name, retention_tier, tenant_id, part, period_month,
		s3_bucket, s3_key, row_count, byte_size, min_occurred_at, max_occurred_at, min_id, max_id, sha256)
		VALUES ('audit_events_2019_03', 'security_3y', $1, 0, '2019-03-01', 'b', $2, 1, 1, '2019-03-01', '2019-03-02',
		        '00000000-0000-0000-0000-000000000000', 'ffffffff-ffff-ffff-ffff-ffffffffffff', 'x')`

	err := inTx(withTenant(ctx, tenantA), db.appPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, insert, tenantA, "k-app")
		return err
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "permission denied")

	require.NoError(t, inTx(ctx, db.reconPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, insert, tenantB, "k-recon")
		return err
	}), "the reconciler (BYPASSRLS) writes manifest rows for any tenant")
	var n int
	require.NoError(t, inTx(ctx, db.reconPool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_archive_objects`).Scan(&n)
	}))
	assert.Equal(t, 1, n)

	// audit_app: B's row visible only under B's GUC; no GUC → none (fail-closed).
	count := func(c context.Context) int {
		var n int
		require.NoError(t, inTx(c, db.appPool, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM audit_archive_objects`).Scan(&n)
		}))
		return n
	}
	assert.Equal(t, 1, count(withTenant(ctx, tenantB)))
	assert.Equal(t, 0, count(withTenant(ctx, tenantA)))
	assert.Equal(t, 0, count(ctx))
}
