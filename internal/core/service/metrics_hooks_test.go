package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
)

// recMetrics records every metrics-port call (IngestMetrics, QueryMetrics,
// OpsMetrics) so the hooks can be asserted without Prometheus.
type recMetrics struct {
	mu          sync.Mutex
	ingested    []domain.AuditEntry
	duplicates  []string
	unknown     []string
	directWrite []string // "source|result"
	clamped     []string
	archived    int
	exports     []string
	stats       []domain.OpsStats
	dlq         map[string]int64
	queue       map[string]int64
	rls         map[string]int64
	dupMessages []string
}

func (m *recMetrics) Ingested(e domain.AuditEntry) { m.ingested = append(m.ingested, e) }
func (m *recMetrics) Duplicate(c string)           { m.duplicates = append(m.duplicates, c) }
func (m *recMetrics) Unknown(s string)             { m.unknown = append(m.unknown, s) }
func (m *recMetrics) DuplicateMessage(et string)   { m.dupMessages = append(m.dupMessages, et) }
func (m *recMetrics) DirectWrite(s, r string)      { m.directWrite = append(m.directWrite, s+"|"+r) }
func (m *recMetrics) WindowClamped(p string)       { m.clamped = append(m.clamped, p) }
func (m *recMetrics) ArchivedRead()                { m.archived++ }
func (m *recMetrics) ExportJob(s string)           { m.exports = append(m.exports, s) }

func (m *recMetrics) SetOpsStats(s domain.OpsStats) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stats = append(m.stats, s)
}

func (m *recMetrics) SetDLQDepth(q string, d int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dlq == nil {
		m.dlq = map[string]int64{}
	}
	m.dlq[q] = d
}

func (m *recMetrics) SetQueueDepth(q string, d int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.queue == nil {
		m.queue = map[string]int64{}
	}
	m.queue[q] = d
}

func (m *recMetrics) AddRLSViolations(vt string, n int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rls == nil {
		m.rls = map[string]int64{}
	}
	m.rls[vt] += n
}

func (m *recMetrics) statCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.stats)
}

var (
	_ port.IngestMetrics = (*recMetrics)(nil)
	_ port.QueryMetrics  = (*recMetrics)(nil)
	_ port.OpsMetrics    = (*recMetrics)(nil)
)

// ── write path ─────────────────────────────────────────────────────────

// AL-5: created → Ingested + DirectWrite(created); a replay → Duplicate
// (direct_write) + DirectWrite(duplicate).
func TestIngestMetrics_DirectWriteCreatedThenDuplicate(t *testing.T) {
	m := &recMetrics{}
	svc := NewIngestService(&fakeStore{}, seqIDs(), 8192, 500, nil).WithMetrics(m)
	if _, created, err := svc.DirectWrite(context.Background(), cmd("k1")); err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if _, created, err := svc.DirectWrite(context.Background(), cmd("k1")); err != nil || created {
		t.Fatalf("replay created=%v err=%v", created, err)
	}
	if len(m.ingested) != 1 || m.ingested[0].EntryType != "config.idp.changed" {
		t.Errorf("ingested = %+v", m.ingested)
	}
	if fmt.Sprint(m.duplicates) != "["+domain.ConsumerDirectWrite+"]" {
		t.Errorf("duplicates = %v", m.duplicates)
	}
	want := "[iam-realm-provisioner|created iam-realm-provisioner|duplicate]"
	if fmt.Sprint(m.directWrite) != want {
		t.Errorf("directWrite = %v, want %s", m.directWrite, want)
	}
}

// directwrite_requests_total result: a 4xx domain error → rejected; a 5xx
// domain error or a plain error → error.
func TestIngestMetrics_DirectWriteRejectedAndError(t *testing.T) {
	m := &recMetrics{}
	svc := NewIngestService(&fakeStore{}, seqIDs(), 8192, 500, nil).WithMetrics(m)
	bad := cmd("k2")
	bad.EntryType = "not.a.known.type"
	if _, _, err := svc.DirectWrite(context.Background(), bad); err == nil {
		t.Fatal("unknown entry type must fail")
	}
	svc = NewIngestService(&fakeStore{err: domain.NewError(domain.ErrDependencyUnavailable, "db")}, seqIDs(), 8192, 500, nil).WithMetrics(m)
	if _, _, err := svc.DirectWrite(context.Background(), cmd("k3")); err == nil {
		t.Fatal("dependency error must fail")
	}
	svc = NewIngestService(&fakeStore{err: errors.New("raw")}, seqIDs(), 8192, 500, nil).WithMetrics(m)
	if _, _, err := svc.DirectWrite(context.Background(), cmd("k4")); err == nil {
		t.Fatal("raw error must fail")
	}
	want := "[iam-realm-provisioner|rejected iam-realm-provisioner|error iam-realm-provisioner|error]"
	if fmt.Sprint(m.directWrite) != want {
		t.Errorf("directWrite = %v, want %s", m.directWrite, want)
	}
	if len(m.ingested) != 0 || len(m.duplicates) != 0 {
		t.Errorf("failures must not count as ingested/duplicate: %+v %v", m.ingested, m.duplicates)
	}
}

func TestDirectWriteResult(t *testing.T) {
	for _, tc := range []struct {
		created bool
		err     error
		want    string
	}{
		{true, nil, "created"},
		{false, nil, "duplicate"},
		{false, domain.NewError(domain.ErrInvalidRequest, "x"), "rejected"},
		{false, domain.NewError(domain.ErrRateLimited, "x"), "rejected"},
		{false, domain.NewError(domain.ErrDependencyUnavailable, "x"), "error"},
		{false, fmt.Errorf("wrapped: %w", domain.NewError(domain.ErrUnknownEntryType, "x")), "rejected"},
		{false, errors.New("raw"), "error"},
	} {
		if got := directWriteResult(tc.created, tc.err); got != tc.want {
			t.Errorf("directWriteResult(%v, %v) = %s, want %s", tc.created, tc.err, got, tc.want)
		}
	}
}

// AL-6: the batch path counts per entry, including a rejected one.
func TestIngestMetrics_BatchCountsPerEntry(t *testing.T) {
	m := &recMetrics{}
	svc := NewIngestService(&fakeStore{}, seqIDs(), 8192, 500, nil).WithMetrics(m)
	bad := cmd("b2")
	bad.EntryType = "not.a.known.type"
	if _, err := svc.DirectWriteBatch(context.Background(), []domain.DirectWriteCommand{cmd("b1"), bad, cmd("b1")}); err != nil {
		t.Fatal(err)
	}
	want := "[iam-realm-provisioner|created iam-realm-provisioner|rejected iam-realm-provisioner|duplicate]"
	if fmt.Sprint(m.directWrite) != want {
		t.Errorf("directWrite = %v, want %s", m.directWrite, want)
	}
	if len(m.ingested) != 1 || len(m.duplicates) != 1 {
		t.Errorf("ingested=%d duplicates=%d", len(m.ingested), len(m.duplicates))
	}
}

// Bus path: created → Ingested; a redelivery → Duplicate(consumer); an
// unknown type counts Unknown only when it is created (not on a replay).
func TestIngestMetrics_BusCreatedDuplicateUnknown(t *testing.T) {
	m := &recMetrics{}
	svc := NewIngestService(&fakeStore{}, seqIDs(), 8192, 500, nil).WithBus(BusIngestConfig{}).WithMetrics(m)
	ctx := context.Background()
	known := busEv(domain.TopicUser, "UserUpdated", `{"user_id":"u"}`)
	unknown := busEv(domain.TopicTender, "TenderShredded", `{}`)
	for _, ev := range []domain.BusEvent{known, known, unknown, unknown} {
		if _, err := svc.IngestBus(ctx, ev, "q"); err != nil {
			t.Fatal(err)
		}
	}
	if len(m.ingested) != 2 {
		t.Errorf("ingested = %d, want 2 (one known, one unknown)", len(m.ingested))
	}
	if fmt.Sprint(m.duplicates) != "[q q]" {
		t.Errorf("duplicates = %v", m.duplicates)
	}
	if len(m.unknown) != 1 {
		t.Errorf("unknown = %v, want exactly one (create only)", m.unknown)
	}
	if len(m.directWrite) != 0 {
		t.Errorf("bus ingest must not count directwrite: %v", m.directWrite)
	}
	// platform_duplicate_messages_total: one per redelivery, event_type
	// bounded to the taxonomy.
	if fmt.Sprint(m.dupMessages) != "[UserUpdated unknown]" {
		t.Errorf("duplicate messages = %v, want [UserUpdated unknown]", m.dupMessages)
	}
}

// No metrics wired → no panic on any path.
func TestIngestMetrics_NilIsSafe(t *testing.T) {
	svc := NewIngestService(&fakeStore{}, seqIDs(), 8192, 500, nil).WithBus(BusIngestConfig{})
	if _, _, err := svc.DirectWrite(context.Background(), cmd("n1")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.IngestBus(context.Background(), busEv(domain.TopicTender, "TenderShredded", `{}`), "q"); err != nil {
		t.Fatal(err)
	}
}

// ── read path ──────────────────────────────────────────────────────────

// WindowClamped fires only for an explicit from that was cut, with the
// tenant's plan code; a defaulted from, or an explicit from inside the
// window, is not counted.
func TestQueryMetrics_WindowClampedExplicitOnly(t *testing.T) {
	m := &recMetrics{}
	r := &fakeReader{plan: &domain.PlanWindowRow{PlanCode: "starter", QueryWindowDays: 30}}
	svc := newQuery(r, nil, QueryConfig{Metrics: m})
	ctx := context.Background()
	old, inside := qNow.AddDate(0, 0, -90), qNow.AddDate(0, 0, -5)
	for _, f := range []domain.QueryFilter{{}, {From: &inside}, {From: &old}} {
		if _, err := svc.Query(ctx, QueryRequest{TenantID: qTenant, Filter: f}); err != nil {
			t.Fatal(err)
		}
	}
	if fmt.Sprint(m.clamped) != "[starter]" {
		t.Errorf("clamped = %v, want [starter]", m.clamped)
	}

	// No tenant_plan_window row → plan code "".
	m2 := &recMetrics{}
	svc2 := newQuery(&fakeReader{}, nil, QueryConfig{Metrics: m2, DefaultWindowDays: 30})
	if _, err := svc2.Query(ctx, QueryRequest{TenantID: qTenant, Filter: domain.QueryFilter{From: &old}}); err != nil {
		t.Fatal(err)
	}
	if len(m2.clamped) != 1 || m2.clamped[0] != "" {
		t.Errorf("no row: clamped = %q", m2.clamped)
	}

	// An explicit range entirely before the window is clamped too.
	m3 := &recMetrics{}
	svc3 := newQuery(&fakeReader{plan: &domain.PlanWindowRow{PlanCode: "pro", QueryWindowDays: 30}}, nil, QueryConfig{Metrics: m3})
	older := qNow.AddDate(0, 0, -60)
	if _, err := svc3.Query(ctx, QueryRequest{TenantID: qTenant, Filter: domain.QueryFilter{From: &old, To: &older}}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(m3.clamped) != "[pro]" {
		t.Errorf("empty-range clamp = %v", m3.clamped)
	}
}

// ArchivedRead: counted when archived objects are read on AL-1, not for a
// hot-only query, and on an AL-2 archive hit (not on an AL-2 hot hit).
func TestQueryMetrics_ArchivedRead(t *testing.T) {
	m := &recMetrics{}
	old := qNow.AddDate(-2, 0, 0)
	arch := entryAt(5, old, domain.TierCompliance7y)
	o := object("near", 1, old, old)
	o.MinID, o.MaxID = qid(5), qid(5)
	hot := entryAt(1, qNow.AddDate(0, 0, -1), domain.TierSecurity3y)
	r := &fakeReader{plan: &domain.PlanWindowRow{QueryWindowDays: 3 * 365}, hot: []domain.AuditEntry{hot}}
	a := &fakeArchive{records: map[string][]domain.AuditEntry{"near": {arch}}}
	svc := newQuery(r, a, QueryConfig{Metrics: m})
	ctx := context.Background()

	if _, err := svc.Query(ctx, QueryRequest{TenantID: qTenant}); err != nil || m.archived != 0 {
		t.Fatalf("hot-only query: archived=%d err=%v", m.archived, err)
	}
	if _, err := svc.Get(ctx, qTenant, qid(1)); err != nil || m.archived != 0 {
		t.Fatalf("hot AL-2 hit: archived=%d err=%v", m.archived, err)
	}
	r.objects = []domain.ArchiveObject{o}
	if _, err := svc.Query(ctx, QueryRequest{TenantID: qTenant}); err != nil || m.archived != 1 {
		t.Fatalf("archived AL-1: archived=%d err=%v", m.archived, err)
	}
	if e, err := svc.Get(ctx, qTenant, qid(5)); err != nil || e.ID != qid(5) || m.archived != 2 {
		t.Fatalf("archived AL-2 hit: e=%+v archived=%d err=%v", e, m.archived, err)
	}
	if _, err := svc.Get(ctx, qTenant, qid(6)); codeOfErr(err) != domain.ErrAuditEntryNotFound || m.archived != 2 {
		t.Errorf("AL-2 archive miss must not count: archived=%d err=%v", m.archived, err)
	}
}

// export_jobs_total: pending on request, ready on completion, failed on a
// job failure, expired on a lapsed poll.
func TestExportMetrics_Lifecycle(t *testing.T) {
	fx := newExportFixture(t)
	m := &recMetrics{}
	fx.svc.cfg.Metrics = m
	ctx := context.Background()

	if _, err := fx.svc.Request(ctx, qTenant, qUser, domain.QueryFilter{}); err != nil {
		t.Fatal(err)
	}
	from, to := qNow.AddDate(0, -1, 0), qNow
	fx.jobs.claim = clampedJob(qid(1), from, to)
	if worked, err := fx.svc.ProcessNext(ctx); !worked || err != nil {
		t.Fatalf("ready: worked=%v err=%v", worked, err)
	}
	fx.jobs.claim = clampedJob(qid(2), from, to)
	fx.reader.pageErr = errors.New("db")
	if worked, err := fx.svc.ProcessNext(ctx); !worked || err != nil {
		t.Fatalf("failed: worked=%v err=%v", worked, err)
	}
	past := qNow.Add(-time.Minute)
	fx.jobs.jobs[qid(3)] = domain.ExportJob{ID: qid(3), Status: domain.ExportReady, S3Key: "k", SignedURLExpiresAt: &past}
	if v, err := fx.svc.Status(ctx, qTenant, qid(3)); err != nil || v.Job.Status != domain.ExportExpired {
		t.Fatalf("expired: v=%+v err=%v", v, err)
	}
	if want := "[pending ready failed expired]"; fmt.Sprint(m.exports) != want {
		t.Errorf("exports = %v, want %s", m.exports, want)
	}
}

// ── ops monitor ────────────────────────────────────────────────────────

type fakeOpsStats struct {
	mu     sync.Mutex
	stats  domain.OpsStats
	err    error
	got    domain.OpsStatsQuery
	calls  int
	rls    map[string]int64
	rlsErr error
}

func (f *fakeOpsStats) RLSViolations(context.Context) (map[string]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rls, f.rlsErr
}

func (f *fakeOpsStats) OpsStats(_ context.Context, q domain.OpsStatsQuery) (domain.OpsStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.got = q
	return f.stats, f.err
}

type fakeDepths struct {
	depth map[string]int64
	err   map[string]error
}

func (f *fakeDepths) Depth(_ context.Context, url string) (int64, error) {
	if err := f.err[url]; err != nil {
		return 0, err
	}
	return f.depth[url], nil
}

// Collect publishes the stats, and for each inbound queue its depth and its
// DLQ's depth (platform_queue_depth / platform_dlq_depth); the query is
// passed as configured.
func TestOpsMonitor_CollectSetsStatsAndDepths(t *testing.T) {
	st := &fakeOpsStats{stats: domain.OpsStats{DefaultPartitionRows: 2, StalledPartitions: 1, ArchiveLagSeconds: 3600, PendingRedactions: 4}}
	q := &fakeDepths{depth: map[string]int64{"u": 7, "u-dlq": 3, "a": 0, "a-dlq": 0}}
	m, log := &recMetrics{}, &recLog{}
	cfg := OpsMonitorConfig{
		Interval: time.Second,
		Query:    domain.OpsStatsQuery{HotWindowDays: 90, WritableTrailingMonths: 3, StallGrace: time.Hour, PendingAge: time.Minute},
		Queues: []OpsQueue{
			{Name: "user-audit-q", URL: "u", DLQURL: "u-dlq"},
			{Name: "auth-audit-q", URL: "a", DLQURL: "a-dlq"},
		},
	}
	NewOpsMonitor(st, q, m, cfg, log).Collect(context.Background())
	if len(m.stats) != 1 || m.stats[0] != st.stats {
		t.Errorf("stats = %+v", m.stats)
	}
	if st.got != cfg.Query {
		t.Errorf("query = %+v, want %+v", st.got, cfg.Query)
	}
	if m.queue["user-audit-q"] != 7 || m.queue["auth-audit-q"] != 0 || len(m.queue) != 2 {
		t.Errorf("queue depth = %v", m.queue)
	}
	if m.dlq["user-audit-q"] != 3 || m.dlq["auth-audit-q"] != 0 || len(m.dlq) != 2 {
		t.Errorf("dlq depth = %v (keyed by the source queue)", m.dlq)
	}
	if log.warns != 0 {
		t.Errorf("warns = %d", log.warns)
	}
}

// RLS violation counts are fed only when > 0, and a read error warns
// without stopping the pass.
func TestOpsMonitor_RLSViolations(t *testing.T) {
	st := &fakeOpsStats{rls: map[string]int64{"cross_tenant_access": 2, "missing_or_invalid_guc": 0}}
	m := &recMetrics{}
	NewOpsMonitor(st, nil, m, OpsMonitorConfig{}, nil).Collect(context.Background())
	if m.rls["cross_tenant_access"] != 2 || len(m.rls) != 1 {
		t.Errorf("rls = %v, want only cross_tenant_access=2", m.rls)
	}
	st.rlsErr = errors.New("db down")
	m2, log := &recMetrics{}, &recLog{}
	NewOpsMonitor(st, nil, m2, OpsMonitorConfig{}, log).Collect(context.Background())
	if len(m2.rls) != 0 || len(m2.stats) != 1 || log.warns != 1 {
		t.Errorf("rls=%v stats=%d warns=%d", m2.rls, len(m2.stats), log.warns)
	}
}

// A stats error keeps the last value (nothing set) and warns but still
// reads the queues; a depth error skips that gauge only and warns.
func TestOpsMonitor_ErrorsKeepGoing(t *testing.T) {
	st := &fakeOpsStats{err: errors.New("db down")}
	q := &fakeDepths{depth: map[string]int64{"ok": 1, "ok-dlq": 2}, err: map[string]error{"bad": errors.New("sqs"), "bad-dlq": errors.New("sqs")}}
	m, log := &recMetrics{}, &recLog{}
	NewOpsMonitor(st, q, m, OpsMonitorConfig{Queues: []OpsQueue{
		{Name: "a", URL: "ok", DLQURL: "ok-dlq"},
		{Name: "b", URL: "bad", DLQURL: "bad-dlq"},
	}}, log).Collect(context.Background())
	if len(m.stats) != 0 {
		t.Errorf("stats must not be set on error: %+v", m.stats)
	}
	if m.queue["a"] != 1 || m.dlq["a"] != 2 || len(m.queue) != 1 || len(m.dlq) != 1 {
		t.Errorf("queue=%v dlq=%v, want only a", m.queue, m.dlq)
	}
	if log.warns != 3 {
		t.Errorf("warns = %d, want 3 (stats + queue + dlq)", log.warns)
	}
	// A nil logger is tolerated.
	NewOpsMonitor(st, q, m, OpsMonitorConfig{Queues: []OpsQueue{{Name: "b", URL: "bad", DLQURL: "bad-dlq"}}}, nil).Collect(context.Background())
}

// A nil QueueDepths skips the depth gauges entirely.
func TestOpsMonitor_NilQueuesSkipsDepths(t *testing.T) {
	m := &recMetrics{}
	NewOpsMonitor(&fakeOpsStats{}, nil, m, OpsMonitorConfig{Queues: []OpsQueue{{Name: "a", URL: "u", DLQURL: "u-dlq"}}}, nil).Collect(context.Background())
	if len(m.stats) != 1 || len(m.dlq) != 0 || len(m.queue) != 0 {
		t.Errorf("stats=%d dlq=%v queue=%v", len(m.stats), m.dlq, m.queue)
	}
}

// Run collects immediately, keeps collecting on the interval, and stops on
// cancel. The default interval is a minute.
func TestOpsMonitor_RunImmediateThenStops(t *testing.T) {
	if got := NewOpsMonitor(&fakeOpsStats{}, nil, &recMetrics{}, OpsMonitorConfig{}, nil).cfg.Interval; got != time.Minute {
		t.Errorf("default interval = %v", got)
	}
	m := &recMetrics{}
	mon := NewOpsMonitor(&fakeOpsStats{}, nil, m, OpsMonitorConfig{Interval: 10 * time.Millisecond}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		mon.Run(ctx)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for m.statCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if m.statCount() < 2 {
		t.Fatalf("collections = %d, want ≥ 2", m.statCount())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on cancel")
	}
}
