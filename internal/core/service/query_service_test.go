package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
)

const qTenant = "11111111-1111-1111-1111-111111111111"

var qNow = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func qClock() time.Time { return qNow }

// qid returns a canonical lowercase UUID whose string order follows n.
func qid(n int) string { return fmt.Sprintf("0190a1b2-0000-7000-8000-%012d", n) }

// fakeReader is an in-memory port.AuditReader: hot rows filtered/paged like
// the SQL keyset query, archived objects returned verbatim.
type fakeReader struct {
	hot      []domain.AuditEntry
	objects  []domain.ArchiveObject
	plan     *domain.PlanWindowRow
	planErr  error
	pageErr  error
	objErr   error
	getErr   error
	idObjErr error
	gotIDAt  time.Time
	pages    int
	gotFrom  time.Time
	gotTo    time.Time
	gotTier  string
	gotLimit int
}

func (f *fakeReader) Page(_ context.Context, _ string, flt domain.QueryFilter, from, to time.Time, after *domain.Cursor, limit int) ([]domain.AuditEntry, error) {
	f.pages++
	f.gotFrom, f.gotTo, f.gotLimit = from, to, limit
	if f.pageErr != nil {
		return nil, f.pageErr
	}
	rows := append([]domain.AuditEntry(nil), f.hot...)
	sort.Slice(rows, func(i, j int) bool { return domain.NewerFirst(rows[i], rows[j]) })
	var out []domain.AuditEntry
	for _, e := range rows {
		if e.OccurredAt.Before(from) || e.OccurredAt.After(to) || !flt.Matches(e) || !after.After(e) {
			continue
		}
		out = append(out, e)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (f *fakeReader) Get(_ context.Context, _, id string) (domain.AuditEntry, error) {
	if f.getErr != nil {
		return domain.AuditEntry{}, f.getErr
	}
	for _, e := range f.hot {
		if e.ID == id {
			return e, nil
		}
	}
	return domain.AuditEntry{}, port.ErrNotFound
}

func (f *fakeReader) PlanWindow(context.Context, string) (*domain.PlanWindowRow, error) {
	return f.plan, f.planErr
}

func (f *fakeReader) ArchivedObjects(_ context.Context, _ string, _, _ time.Time, tier string) ([]domain.ArchiveObject, error) {
	f.gotTier = tier
	return f.objects, f.objErr
}

// ArchivedObjectsForID returns the objects whose id range holds id.
func (f *fakeReader) ArchivedObjectsForID(_ context.Context, _, id string, from time.Time) ([]domain.ArchiveObject, error) {
	f.gotIDAt = from
	if f.idObjErr != nil {
		return nil, f.idObjErr
	}
	var out []domain.ArchiveObject
	for _, o := range f.objects {
		if o.MinID <= id && id <= o.MaxID {
			out = append(out, o)
		}
	}
	return out, nil
}

// fakeArchive serves records per key; errs forces a per-key error.
type fakeArchive struct {
	records map[string][]domain.AuditEntry
	errs    map[string]error
	reads   []string
}

func (a *fakeArchive) ReadArchive(_ context.Context, _, key string, fn func(domain.ArchiveRecord) error) error {
	a.reads = append(a.reads, key)
	if err := a.errs[key]; err != nil {
		return err
	}
	for _, e := range a.records[key] {
		if err := fn(domain.ToRecord(e)); err != nil {
			return err
		}
	}
	return nil
}

func entryAt(n int, at time.Time, tier domain.RetentionTier) domain.AuditEntry {
	return domain.AuditEntry{
		ID: qid(n), OccurredAt: at, TenantID: qTenant, EntryType: "config.idp.changed", Action: "update",
		Actor: domain.ActorRef{Type: domain.ActorIAMSystem, ID: domain.IAMSystemActorID}, SourceService: "svc",
		RetentionTier: tier, IngestMode: domain.IngestDirectWrite,
	}
}

func object(key string, rows int64, minAt, maxAt time.Time) domain.ArchiveObject {
	return domain.ArchiveObject{Bucket: "b", Key: key, TenantID: qTenant, RowCount: rows, ByteSize: rows * 100,
		MinOccurredAt: minAt, MaxOccurredAt: maxAt}
}

func newQuery(r *fakeReader, a *fakeArchive, cfg QueryConfig) *QueryService {
	if cfg.DefaultWindowDays == 0 {
		cfg.DefaultWindowDays = 365
	}
	cfg.Now = qClock
	var archive port.ArchiveReader
	if a != nil {
		archive = a
	}
	return NewQueryService(r, archive, cfg, &recLog{})
}

func TestQuery_RejectsBadLimitCursorFilter(t *testing.T) {
	svc := newQuery(&fakeReader{}, nil, QueryConfig{})
	for name, req := range map[string]QueryRequest{
		"limit too big":  {Limit: domain.MaxQueryLimit + 1},
		"limit negative": {Limit: -1},
		"bad cursor":     {Cursor: "garbage!"},
		"bad filter":     {Filter: domain.QueryFilter{ActorType: "robot"}},
	} {
		req.TenantID = qTenant
		if _, err := svc.Query(context.Background(), req); codeOfErr(err) != domain.ErrInvalidRequest {
			t.Errorf("%s: err = %v, want invalid_request", name, err)
		}
	}
}

func codeOfErr(err error) domain.ErrorCode {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.Code
	}
	return ""
}

func TestQuery_ReaderErrorsPropagate(t *testing.T) {
	boom := errors.New("db down")
	for name, r := range map[string]*fakeReader{
		"plan":    {planErr: boom},
		"objects": {objErr: boom},
		"page":    {pageErr: boom},
	} {
		if _, err := newQuery(r, &fakeArchive{}, QueryConfig{}).Query(context.Background(), QueryRequest{TenantID: qTenant}); !errors.Is(err, boom) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// §5.4: absent from → clamped to earliest; a range entirely before the
// window → 200 empty page, clamped, no store read.
func TestQuery_WindowClampAndEmptyRange(t *testing.T) {
	r := &fakeReader{plan: &domain.PlanWindowRow{PlanCode: "starter", QueryWindowDays: 30}}
	svc := newQuery(r, &fakeArchive{}, QueryConfig{})
	res, err := svc.Query(context.Background(), QueryRequest{TenantID: qTenant})
	if err != nil {
		t.Fatal(err)
	}
	earliest := qNow.AddDate(0, 0, -30)
	if !res.WindowClamped || !res.EffectiveFrom.Equal(earliest) || !r.gotFrom.Equal(earliest) || !r.gotTo.Equal(qNow) {
		t.Errorf("clamp: res=%+v from=%v to=%v", res, r.gotFrom, r.gotTo)
	}
	if r.gotLimit != domain.DefaultQueryLimit+1 {
		t.Errorf("default limit: page asked for %d rows", r.gotLimit)
	}

	r.pages = 0
	from, to := qNow.AddDate(0, 0, -90), qNow.AddDate(0, 0, -60)
	res, err = svc.Query(context.Background(), QueryRequest{TenantID: qTenant, Filter: domain.QueryFilter{From: &from, To: &to}})
	if err != nil || !res.WindowClamped || len(res.Events) != 0 || res.NextCursor != "" || r.pages != 0 {
		t.Errorf("empty range: res=%+v err=%v pages=%d", res, err, r.pages)
	}
}

// AL-D15: the live CAT-I2 map value outranks the stored row.
func TestQuery_LiveWindowMapWins(t *testing.T) {
	r := &fakeReader{plan: &domain.PlanWindowRow{PlanCode: "pro", QueryWindowDays: 30}}
	svc := newQuery(r, &fakeArchive{}, QueryConfig{Windows: fakeWindows{"pro": 1095}})
	res, err := svc.Query(context.Background(), QueryRequest{TenantID: qTenant})
	if err != nil || !res.EffectiveFrom.Equal(qNow.AddDate(0, 0, -1095)) {
		t.Errorf("effective_from = %v, err %v", res.EffectiveFrom, err)
	}
}

// AL-INV-8: the plan window is a query-time clamp, independent of the
// retention tier being queried — a compliance_7y filter sees no further
// back than an access_90d one.
func TestQuery_WindowClampIndependentOfTier_ALINV8(t *testing.T) {
	old := qNow.AddDate(-5, 0, 0)
	r := &fakeReader{
		plan: &domain.PlanWindowRow{PlanCode: "starter", QueryWindowDays: 365},
		hot: []domain.AuditEntry{
			entryAt(1, old, domain.TierCompliance7y),
			entryAt(2, qNow.AddDate(0, 0, -10), domain.TierCompliance7y),
			entryAt(3, qNow.AddDate(0, 0, -10), domain.TierAccess90d),
		},
	}
	svc := newQuery(r, &fakeArchive{}, QueryConfig{})
	var froms []time.Time
	for _, tier := range []domain.RetentionTier{domain.TierCompliance7y, domain.TierAccess90d} {
		res, err := svc.Query(context.Background(), QueryRequest{TenantID: qTenant,
			Filter: domain.QueryFilter{From: &old, RetentionTier: string(tier)}})
		if err != nil {
			t.Fatal(err)
		}
		if !res.WindowClamped || len(res.Events) != 1 || res.Events[0].OccurredAt.Before(res.EffectiveFrom) {
			t.Errorf("%s: res=%+v", tier, res)
		}
		froms = append(froms, res.EffectiveFrom)
	}
	if !froms[0].Equal(froms[1]) || !froms[0].Equal(qNow.AddDate(0, 0, -365)) {
		t.Errorf("clamp differs by tier: %v", froms)
	}
}

// §5.1 keyset: pages are disjoint, ordered, and exhaust with a nil cursor.
func TestQuery_HotKeysetPaging(t *testing.T) {
	t0 := qNow.Add(-time.Hour)
	r := &fakeReader{}
	for i := 1; i <= 5; i++ {
		r.hot = append(r.hot, entryAt(i, t0, domain.TierSecurity3y)) // all tied on occurred_at
	}
	svc := newQuery(r, &fakeArchive{}, QueryConfig{})
	var got []string
	cursor := ""
	for page := 0; page < 5; page++ {
		res, err := svc.Query(context.Background(), QueryRequest{TenantID: qTenant, Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range res.Events {
			got = append(got, e.ID)
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	want := []string{qid(5), qid(4), qid(3), qid(2), qid(1)}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("paged ids = %v, want %v", got, want)
	}
}

// D-12: hot and archived months merge into one (occurred_at DESC, id DESC)
// stream, and the cursor carries across the source boundary.
func TestQuery_HotAndArchivedMergeAcrossCursor_D12(t *testing.T) {
	r := &fakeReader{plan: &domain.PlanWindowRow{PlanCode: "ent", QueryWindowDays: 3650}}
	r.hot = []domain.AuditEntry{
		entryAt(10, qNow.AddDate(0, 0, -1), domain.TierSecurity3y),
		entryAt(11, qNow.AddDate(0, 0, -2), domain.TierSecurity3y),
	}
	m1a, m1b := qNow.AddDate(0, -6, 0), qNow.AddDate(0, -6, -1)
	m2a := qNow.AddDate(0, -8, 0)
	outOfRange := qNow.AddDate(-20, 0, 0) // inside the object, outside the window
	a := &fakeArchive{records: map[string][]domain.AuditEntry{
		"k1": {entryAt(20, m1b, domain.TierSecurity3y), entryAt(21, m1a, domain.TierSecurity3y)}, // unsorted on purpose
		"k2": {entryAt(30, m2a, domain.TierSecurity3y), entryAt(31, outOfRange, domain.TierSecurity3y)},
	}}
	r.objects = []domain.ArchiveObject{object("k1", 2, m1b, m1a), object("k2", 2, outOfRange, m2a)}
	svc := newQuery(r, a, QueryConfig{SyncMaxRows: 100, SyncMaxBytes: 1 << 20})

	var got []string
	cursor := ""
	for i := 0; i < 10; i++ {
		res, err := svc.Query(context.Background(), QueryRequest{TenantID: qTenant, Limit: 2, Cursor: cursor, Filter: domain.QueryFilter{RetentionTier: "security_3y"}})
		if err != nil {
			t.Fatal(err)
		}
		if res.Deferred != nil {
			t.Fatal("within bounds must not defer")
		}
		for _, e := range res.Events {
			got = append(got, e.ID)
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	want := []string{qid(10), qid(11), qid(21), qid(20), qid(30)}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("merged ids = %v, want %v", got, want)
	}
	if r.gotTier != "security_3y" {
		t.Errorf("tier not passed to manifest lookup: %q", r.gotTier)
	}
}

// readArchived skips objects wholly newer than the cursor and stops once no
// remaining object can contribute.
func TestQuery_ArchivedSkipsAndEarlyStop(t *testing.T) {
	newer, mid, oldest := qNow.AddDate(0, -4, 0), qNow.AddDate(0, -5, 0), qNow.AddDate(0, -7, 0)
	a := &fakeArchive{records: map[string][]domain.AuditEntry{
		"new": {entryAt(1, newer, domain.TierSecurity3y)},
		"mid": {entryAt(2, mid, domain.TierSecurity3y), entryAt(3, mid.Add(-time.Hour), domain.TierSecurity3y)},
		"old": {entryAt(4, oldest, domain.TierSecurity3y)},
	}}
	r := &fakeReader{plan: &domain.PlanWindowRow{QueryWindowDays: 3650}, objects: []domain.ArchiveObject{
		object("new", 1, newer, newer), object("mid", 2, mid.Add(-time.Hour), mid), object("old", 1, oldest, oldest),
	}}
	svc := newQuery(r, a, QueryConfig{})
	cursor := domain.Cursor{OccurredAt: newer.Add(-time.Minute), ID: qid(9)}.Encode()
	res, err := svc.Query(context.Background(), QueryRequest{TenantID: qTenant, Limit: 1, Cursor: cursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 1 || res.Events[0].ID != qid(2) || res.NextCursor == "" {
		t.Errorf("res = %+v", res)
	}
	if fmt.Sprint(a.reads) != "[mid]" {
		t.Errorf("objects read = %v, want only [mid]", a.reads)
	}
}

// D-10: an archived read estimated over either bound becomes a deferred,
// clamped export filter — no rows are read.
func TestQuery_DefersLargeArchivedRead_D10(t *testing.T) {
	at := qNow.AddDate(0, -6, 0)
	for name, cfg := range map[string]QueryConfig{
		"rows":  {SyncMaxRows: 10, SyncMaxBytes: 1 << 30},
		"bytes": {SyncMaxRows: 1 << 30, SyncMaxBytes: 1000},
	} {
		r := &fakeReader{objects: []domain.ArchiveObject{object("k", 11, at, at)}}
		a := &fakeArchive{}
		res, err := newQuery(r, a, cfg).Query(context.Background(), QueryRequest{TenantID: qTenant,
			Filter: domain.QueryFilter{ActorType: "user"}})
		if err != nil {
			t.Fatal(err)
		}
		if res.Deferred == nil || res.Deferred.From == nil || res.Deferred.To == nil || res.Deferred.ActorType != "user" {
			t.Fatalf("%s: deferred = %+v", name, res.Deferred)
		}
		if !res.Deferred.From.Equal(qNow.AddDate(0, 0, -365)) || !res.Deferred.To.Equal(qNow) {
			t.Errorf("%s: deferred range %v..%v", name, res.Deferred.From, res.Deferred.To)
		}
		if r.pages != 0 || len(a.reads) != 0 {
			t.Errorf("%s: deferral must not read (pages=%d reads=%v)", name, r.pages, a.reads)
		}
	}
	// Unbounded config (0) never defers.
	r := &fakeReader{objects: []domain.ArchiveObject{object("k", 1<<40, at, at)}}
	if res, err := newQuery(r, &fakeArchive{}, QueryConfig{}).Query(context.Background(), QueryRequest{TenantID: qTenant}); err != nil || res.Deferred != nil {
		t.Errorf("zero bounds: res=%+v err=%v", res, err)
	}
}

func TestQuery_MissingArchiveObjectSkippedOtherErrorsFail(t *testing.T) {
	at := qNow.AddDate(0, -6, 0)
	r := &fakeReader{objects: []domain.ArchiveObject{object("gone", 1, at, at), object("ok", 1, at.Add(-time.Hour), at.Add(-time.Hour))}}
	a := &fakeArchive{
		records: map[string][]domain.AuditEntry{"ok": {entryAt(1, at.Add(-time.Hour), domain.TierSecurity3y)}},
		errs:    map[string]error{"gone": fmt.Errorf("%w: nsk", port.ErrObjectMissing)},
	}
	log := &recLog{}
	svc := NewQueryService(r, a, QueryConfig{DefaultWindowDays: 365, Now: qClock}, log)
	res, err := svc.Query(context.Background(), QueryRequest{TenantID: qTenant})
	if err != nil || len(res.Events) != 1 || log.warns != 1 {
		t.Errorf("res=%+v err=%v warns=%d", res, err, log.warns)
	}
	// A nil logger is tolerated.
	if _, err := NewQueryService(r, a, QueryConfig{DefaultWindowDays: 365, Now: qClock}, nil).Query(context.Background(), QueryRequest{TenantID: qTenant}); err != nil {
		t.Errorf("nil logger: %v", err)
	}

	boom := domain.NewError(domain.ErrDependencyUnavailable, "s3 down")
	a.errs["gone"] = boom
	if _, err := svc.Query(context.Background(), QueryRequest{TenantID: qTenant}); !errors.Is(err, boom) {
		t.Errorf("archive error must propagate, got %v", err)
	}
}

func TestNewQueryService_DefaultsClock(t *testing.T) {
	svc := NewQueryService(&fakeReader{}, nil, QueryConfig{DefaultWindowDays: 1}, nil)
	res, err := svc.Query(context.Background(), QueryRequest{TenantID: qTenant})
	if err != nil || time.Since(res.EffectiveFrom) < 23*time.Hour {
		t.Errorf("real clock: res=%+v err=%v", res, err)
	}
}

// AL-2: within tenant and window; outside the window is a 404.
func TestQuery_Get(t *testing.T) {
	r := &fakeReader{
		plan: &domain.PlanWindowRow{QueryWindowDays: 30},
		hot:  []domain.AuditEntry{entryAt(1, qNow.AddDate(0, 0, -1), domain.TierSecurity3y), entryAt(2, qNow.AddDate(0, 0, -60), domain.TierCompliance7y)},
	}
	svc := newQuery(r, nil, QueryConfig{})
	if e, err := svc.Get(context.Background(), qTenant, qid(1)); err != nil || e.ID != qid(1) {
		t.Errorf("in window: %+v %v", e, err)
	}
	for _, id := range []string{qid(2), qid(99)} {
		if _, err := svc.Get(context.Background(), qTenant, id); codeOfErr(err) != domain.ErrAuditEntryNotFound {
			t.Errorf("%s: err = %v, want audit_entry_not_found", id, err)
		}
	}
	if _, err := svc.Get(context.Background(), qTenant, "nope"); codeOfErr(err) != domain.ErrInvalidRequest {
		t.Errorf("bad id: %v", err)
	}
	boom := errors.New("db")
	r.getErr = boom
	if _, err := svc.Get(context.Background(), qTenant, qid(1)); !errors.Is(err, boom) {
		t.Errorf("get error: %v", err)
	}
	r.planErr = boom
	if _, err := svc.Get(context.Background(), qTenant, qid(1)); !errors.Is(err, boom) {
		t.Errorf("plan error: %v", err)
	}
}

// Gap 32: an entry in a dropped (archived) month is still readable via AL-2
// — only objects whose id range holds the id are read — and the window
// clamp still applies to it.
func TestQuery_GetFallsBackToArchive(t *testing.T) {
	old := qNow.AddDate(-2, 0, 0)
	inWindow := entryAt(5, old, domain.TierCompliance7y)
	outside := entryAt(6, qNow.AddDate(-9, 0, 0), domain.TierCompliance7y)
	near := object("near", 2, old.Add(-time.Hour), old)
	near.MinID, near.MaxID = qid(5), qid(6)
	far := object("far", 1, old, old)
	far.MinID, far.MaxID = qid(50), qid(60)
	gone := object("gone", 1, old, old)
	gone.MinID, gone.MaxID = qid(1), qid(9)
	r := &fakeReader{plan: &domain.PlanWindowRow{QueryWindowDays: 3 * 365}, objects: []domain.ArchiveObject{gone, far, near}}
	a := &fakeArchive{
		records: map[string][]domain.AuditEntry{"near": {outside, inWindow}, "far": {entryAt(55, old, domain.TierSecurity3y)}},
		errs:    map[string]error{"gone": port.ErrObjectMissing},
	}
	svc := newQuery(r, a, QueryConfig{})

	e, err := svc.Get(context.Background(), qTenant, qid(5))
	if err != nil || e.ID != qid(5) || !e.OccurredAt.Equal(old) {
		t.Fatalf("archived get: %+v %v", e, err)
	}
	if fmt.Sprint(a.reads) != "[gone near]" {
		t.Errorf("reads = %v, want only id-range objects [gone near]", a.reads)
	}
	if !r.gotIDAt.Equal(qNow.AddDate(0, 0, -3*365)) {
		t.Errorf("manifest lookup from = %v, want the window start", r.gotIDAt)
	}
	for _, id := range []string{qid(6), qid(7), qid(99)} { // outside window; in range but absent; no object
		if _, err := svc.Get(context.Background(), qTenant, id); codeOfErr(err) != domain.ErrAuditEntryNotFound {
			t.Errorf("%s: err = %v, want audit_entry_not_found", id, err)
		}
	}

	boom := domain.NewError(domain.ErrDependencyUnavailable, "s3 down")
	a.errs["near"] = boom
	if _, err := svc.Get(context.Background(), qTenant, qid(5)); !errors.Is(err, boom) {
		t.Errorf("archive error must propagate, got %v", err)
	}
	dbErr := errors.New("db")
	r.idObjErr = dbErr
	if _, err := svc.Get(context.Background(), qTenant, qid(5)); !errors.Is(err, dbErr) {
		t.Errorf("manifest error must propagate, got %v", err)
	}
}

func TestMergeNewestFirst_Interleaves(t *testing.T) {
	t0 := qNow
	a := []domain.AuditEntry{entryAt(1, t0, domain.TierSecurity3y), entryAt(3, t0.Add(-2*time.Hour), domain.TierSecurity3y)}
	b := []domain.AuditEntry{entryAt(2, t0.Add(-time.Hour), domain.TierSecurity3y), entryAt(4, t0.Add(-3*time.Hour), domain.TierSecurity3y)}
	var got []string
	for _, e := range mergeNewestFirst(a, b) {
		got = append(got, e.ID)
	}
	if want := []string{qid(1), qid(2), qid(3), qid(4)}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("merged = %v, want %v", got, want)
	}
}
