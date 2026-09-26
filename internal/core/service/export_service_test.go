package service

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
)

const qUser = "7f3c2d1e-9a8b-4c5d-8e7f-0a1b2c3d4e5f"

// fakeJobs is an in-memory port.ExportJobs.
type fakeJobs struct {
	mu        sync.Mutex
	jobs      map[string]domain.ExportJob
	claim     *domain.ExportJob
	claimErr  error
	createErr error
	getErr    error
	doneErr   error
	expireErr error
	created   []domain.ExportJob
	expired   []string
	failed    map[string]string
	completed map[string]domain.ExportJob
	beats     int
}

func newFakeJobs() *fakeJobs {
	return &fakeJobs{jobs: map[string]domain.ExportJob{}, failed: map[string]string{}, completed: map[string]domain.ExportJob{}}
}

func (f *fakeJobs) Create(_ context.Context, j domain.ExportJob) (domain.ExportJob, error) {
	if f.createErr != nil {
		return domain.ExportJob{}, f.createErr
	}
	j.Status, j.CreatedAt = domain.ExportPending, qNow
	f.created = append(f.created, j)
	f.jobs[j.ID] = j
	return j, nil
}

func (f *fakeJobs) Get(_ context.Context, _, id string) (domain.ExportJob, error) {
	if f.getErr != nil {
		return domain.ExportJob{}, f.getErr
	}
	j, ok := f.jobs[id]
	if !ok {
		return domain.ExportJob{}, port.ErrNotFound
	}
	return j, nil
}

func (f *fakeJobs) MarkExpired(_ context.Context, _, id string) error {
	f.expired = append(f.expired, id)
	return f.expireErr
}

func (f *fakeJobs) Claim(context.Context, time.Duration) (*domain.ExportJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := f.claim
	f.claim = nil
	return j, f.claimErr
}

func (f *fakeJobs) Heartbeat(context.Context, string, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.beats++
	return errors.New("heartbeat fails but is only logged")
}

func (f *fakeJobs) Complete(_ context.Context, _, id, key string, rows int64, exp time.Time) error {
	if f.doneErr != nil {
		return f.doneErr
	}
	f.completed[id] = domain.ExportJob{ID: id, S3Key: key, RowCount: &rows, SignedURLExpiresAt: &exp}
	return nil
}

func (f *fakeJobs) Fail(_ context.Context, _, id, reason string) error {
	f.failed[id] = reason
	return errors.New("recording failure fails too — logged")
}

// fakeExportStore captures uploads and presigns.
type fakeExportStore struct {
	puts     map[string][]byte
	putErr   error
	signErr  error
	ttl      time.Duration
	signedAt string
}

func (s *fakeExportStore) PutExport(_ context.Context, key string, body io.ReadSeeker, size int64) error {
	if s.putErr != nil {
		return s.putErr
	}
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if int64(len(b)) != size {
		return fmt.Errorf("size %d != body %d", size, len(b))
	}
	if s.puts == nil {
		s.puts = map[string][]byte{}
	}
	s.puts[key] = b
	return nil
}

func (s *fakeExportStore) PresignExport(_ context.Context, key string, ttl time.Duration) (string, error) {
	s.ttl, s.signedAt = ttl, key
	return "https://signed/" + key, s.signErr
}

type exportFixture struct {
	reader  *fakeReader
	archive *fakeArchive
	jobs    *fakeJobs
	store   *fakeExportStore
	svc     *ExportService
}

func newExportFixture(t *testing.T) *exportFixture {
	t.Helper()
	fx := &exportFixture{
		reader:  &fakeReader{plan: &domain.PlanWindowRow{QueryWindowDays: 30}},
		archive: &fakeArchive{records: map[string][]domain.AuditEntry{}},
		jobs:    newFakeJobs(),
		store:   &fakeExportStore{},
	}
	q := NewQueryService(fx.reader, fx.archive, QueryConfig{DefaultWindowDays: 365, Now: qClock}, &recLog{})
	fx.svc = NewExportService(fx.jobs, fx.reader, fx.archive, fx.store, q, func() (string, error) { return qid(500), nil },
		ExportConfig{SignedURLTTL: 7 * 24 * time.Hour, DownloadURLTTL: 15 * time.Minute, WorkDir: t.TempDir(), Now: qClock}, &recLog{})
	return fx
}

// AL-3 / AL-INV-8: the stored filter is clamped to the plan window.
func TestExport_RequestClampsAndStoresFilter_ALINV8(t *testing.T) {
	fx := newExportFixture(t)
	old := qNow.AddDate(-3, 0, 0)
	job, err := fx.svc.Request(context.Background(), qTenant, qUser, domain.QueryFilter{From: &old, RetentionTier: "compliance_7y"})
	if err != nil {
		t.Fatal(err)
	}
	if job.ID != qid(500) || job.Status != domain.ExportPending || job.RequestedBy != qUser || job.TenantID != qTenant {
		t.Errorf("job = %+v", job)
	}
	f := fx.jobs.created[0].Filter
	if f.From == nil || !f.From.Equal(qNow.AddDate(0, 0, -30)) || f.To == nil || !f.To.Equal(qNow) || f.RetentionTier != "compliance_7y" {
		t.Errorf("stored filter = %+v", f)
	}
}

// Gap 31: no dedup — each AL-3 call is a new job (abuse is the limiter's job).
func TestExport_RequestAlwaysCreatesAndPropagatesErrors(t *testing.T) {
	fx := newExportFixture(t)
	for range 2 {
		if _, err := fx.svc.Request(context.Background(), qTenant, qUser, domain.QueryFilter{}); err != nil {
			t.Fatalf("request: %v", err)
		}
	}
	if len(fx.jobs.created) != 2 {
		t.Errorf("identical requests must create distinct jobs, got %+v", fx.jobs.created)
	}

	boom := errors.New("db")
	fx.jobs.createErr = boom
	if _, err := fx.svc.Request(context.Background(), qTenant, qUser, domain.QueryFilter{}); !errors.Is(err, boom) {
		t.Errorf("create error: %v", err)
	}
	fx.jobs.createErr, fx.reader.planErr = nil, boom
	if _, err := fx.svc.Request(context.Background(), qTenant, qUser, domain.QueryFilter{}); !errors.Is(err, boom) {
		t.Errorf("plan error: %v", err)
	}
	fx.reader.planErr = nil
	if _, err := fx.svc.Request(context.Background(), qTenant, qUser, domain.QueryFilter{ActorID: "x"}); codeOfErr(err) != domain.ErrInvalidRequest {
		t.Errorf("invalid filter: %v", err)
	}
	fx.svc.newID = func() (string, error) { return "", boom }
	if _, err := fx.svc.Request(context.Background(), qTenant, qUser, domain.QueryFilter{}); !errors.Is(err, boom) {
		t.Errorf("id error: %v", err)
	}
}

// AL-4 / D-11: a fresh short-lived URL per poll, never outliving the
// retrieval window; a lapsed job is reported and marked expired.
func TestExport_Status_D11(t *testing.T) {
	fx := newExportFixture(t)
	far, near, past := qNow.Add(48*time.Hour), qNow.Add(5*time.Minute), qNow.Add(-time.Minute)
	fx.jobs.jobs[qid(1)] = domain.ExportJob{ID: qid(1), Status: domain.ExportReady, S3Key: "k1", SignedURLExpiresAt: &far}
	fx.jobs.jobs[qid(2)] = domain.ExportJob{ID: qid(2), Status: domain.ExportReady, S3Key: "k2", SignedURLExpiresAt: &near}
	fx.jobs.jobs[qid(3)] = domain.ExportJob{ID: qid(3), Status: domain.ExportReady, S3Key: "k3", SignedURLExpiresAt: &past}
	fx.jobs.jobs[qid(4)] = domain.ExportJob{ID: qid(4), Status: domain.ExportPending}

	v, err := fx.svc.Status(context.Background(), qTenant, qid(1))
	if err != nil || v.DownloadURL != "https://signed/k1" || fx.store.ttl != 15*time.Minute || !v.DownloadURLExpiresAt.Equal(qNow.Add(15*time.Minute)) {
		t.Errorf("ready: v=%+v ttl=%v err=%v", v, fx.store.ttl, err)
	}
	v, err = fx.svc.Status(context.Background(), qTenant, qid(2))
	if err != nil || fx.store.ttl != 5*time.Minute || !v.DownloadURLExpiresAt.Equal(near) {
		t.Errorf("ttl must be capped by the retrieval window: ttl=%v v=%+v", fx.store.ttl, v)
	}
	v, err = fx.svc.Status(context.Background(), qTenant, qid(3))
	if err != nil || v.Job.Status != domain.ExportExpired || v.DownloadURL != "" || fmt.Sprint(fx.jobs.expired) != fmt.Sprint([]string{qid(3)}) {
		t.Errorf("lapsed: v=%+v expired=%v err=%v", v, fx.jobs.expired, err)
	}
	fx.jobs.expireErr = errors.New("db") // only logged
	if v, err := fx.svc.Status(context.Background(), qTenant, qid(3)); err != nil || v.Job.Status != domain.ExportExpired {
		t.Errorf("mark-expired failure is not fatal: v=%+v err=%v", v, err)
	}
	if v, err := fx.svc.Status(context.Background(), qTenant, qid(4)); err != nil || v.Job.Status != domain.ExportPending || v.DownloadURL != "" {
		t.Errorf("pending: v=%+v err=%v", v, err)
	}
	if _, err := fx.svc.Status(context.Background(), qTenant, qid(9)); codeOfErr(err) != domain.ErrExportNotFound {
		t.Errorf("missing: %v", err)
	}
	if _, err := fx.svc.Status(context.Background(), qTenant, "bad"); codeOfErr(err) != domain.ErrInvalidRequest {
		t.Errorf("bad id: %v", err)
	}
	boom := errors.New("s3")
	fx.store.signErr = boom
	if _, err := fx.svc.Status(context.Background(), qTenant, qid(1)); !errors.Is(err, boom) {
		t.Errorf("presign error: %v", err)
	}
	fx.jobs.getErr = boom
	if _, err := fx.svc.Status(context.Background(), qTenant, qid(1)); !errors.Is(err, boom) {
		t.Errorf("get error: %v", err)
	}
}

func clampedJob(id string, from, to time.Time) *domain.ExportJob {
	return &domain.ExportJob{ID: id, TenantID: qTenant, RequestedBy: qUser, Status: domain.ExportRunning,
		Filter: domain.QueryFilter{From: &from, To: &to}}
}

func decodeExport(t *testing.T, b []byte) []domain.ArchiveRecord {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	var out []domain.ArchiveRecord
	sc := bufio.NewScanner(zr)
	for sc.Scan() {
		var r domain.ArchiveRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func TestExport_ProcessNext_NoJobOrClaimError(t *testing.T) {
	fx := newExportFixture(t)
	if worked, err := fx.svc.ProcessNext(context.Background()); worked || err != nil {
		t.Errorf("idle: worked=%v err=%v", worked, err)
	}
	boom := errors.New("db")
	fx.jobs.claimErr = boom
	if worked, err := fx.svc.ProcessNext(context.Background()); worked || !errors.Is(err, boom) {
		t.Errorf("claim error: worked=%v err=%v", worked, err)
	}
}

// §5.4 AL-3: hot rows (paged past one batch) + archived rows within the
// clamped range land in one gzipped JSONL object; the job is completed.
func TestExport_ProcessNext_StreamsHotAndArchived(t *testing.T) {
	fx := newExportFixture(t)
	from, to := qNow.AddDate(-1, 0, 0), qNow
	for i := 0; i < exportBatch+5; i++ {
		fx.reader.hot = append(fx.reader.hot, entryAt(i+1, qNow.Add(-time.Duration(i+1)*time.Minute), domain.TierSecurity3y))
	}
	inRange, outRange := qNow.AddDate(0, -6, 0), qNow.AddDate(-2, 0, 0)
	fx.archive.records["a1"] = []domain.AuditEntry{entryAt(9001, inRange, domain.TierSecurity3y), entryAt(9002, outRange, domain.TierSecurity3y)}
	fx.archive.records["a2"] = []domain.AuditEntry{entryAt(9003, inRange, domain.TierCompliance7y)}
	fx.archive.errs = map[string]error{"gone": port.ErrObjectMissing}
	fx.reader.objects = []domain.ArchiveObject{object("a1", 2, outRange, inRange), object("gone", 1, inRange, inRange), object("a2", 1, inRange, inRange)}
	fx.jobs.claim = clampedJob(qid(600), from, to)
	fx.jobs.claim.Filter.RetentionTier = "security_3y"

	worked, err := fx.svc.ProcessNext(context.Background())
	if !worked || err != nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	done, ok := fx.jobs.completed[qid(600)]
	key := domain.ExportKey(qTenant, qid(600))
	if !ok || done.S3Key != key || *done.RowCount != exportBatch+5+1 || !done.SignedURLExpiresAt.Equal(qNow.Add(7*24*time.Hour)) {
		t.Fatalf("completed = %+v (ok=%v)", done, ok)
	}
	recs := decodeExport(t, fx.store.puts[key])
	if len(recs) != exportBatch+6 || recs[0].ID != qid(1) || recs[len(recs)-1].ID != qid(9001) {
		t.Errorf("records: n=%d first=%s last=%s", len(recs), recs[0].ID, recs[len(recs)-1].ID)
	}
	if fx.reader.pages != 2 {
		t.Errorf("hot pages = %d, want 2 (one full batch + remainder)", fx.reader.pages)
	}
}

// A job's own failure is recorded on the job with its §17 code, or the
// generic internal_error marker — never the raw cause.
func TestExport_ProcessNext_FailureRecorded(t *testing.T) {
	from, to := qNow.AddDate(0, -1, 0), qNow
	cases := []struct {
		name   string
		mutate func(fx *exportFixture)
		want   string
	}{
		{"s3 put classified", func(fx *exportFixture) {
			fx.store.putErr = domain.NewError(domain.ErrDependencyUnavailable, "s3 down")
		}, "dependency_unavailable"},
		{"hot page raw error", func(fx *exportFixture) { fx.reader.pageErr = errors.New("secret db detail") }, "internal_error"},
		{"manifest error", func(fx *exportFixture) { fx.reader.objErr = errors.New("x") }, "internal_error"},
		{"archive read error", func(fx *exportFixture) {
			at := qNow.AddDate(0, 0, -3)
			fx.reader.objects = []domain.ArchiveObject{object("bad", 1, at, at)}
			fx.archive.errs = map[string]error{"bad": errors.New("corrupt")}
		}, "internal_error"},
		{"bad work dir", func(fx *exportFixture) { fx.svc.cfg.WorkDir = "/nonexistent/dir/for/export" }, "internal_error"},
	}
	for _, tc := range cases {
		fx := newExportFixture(t)
		tc.mutate(fx)
		fx.jobs.claim = clampedJob(qid(1), from, to)
		worked, err := fx.svc.ProcessNext(context.Background())
		if !worked || err != nil {
			t.Errorf("%s: worked=%v err=%v", tc.name, worked, err)
		}
		if got := fx.jobs.failed[qid(1)]; got != tc.want {
			t.Errorf("%s: failure reason %q, want %q", tc.name, got, tc.want)
		}
	}

	fx := newExportFixture(t)
	fx.jobs.claim = &domain.ExportJob{ID: qid(2), TenantID: qTenant} // unclamped filter
	if _, err := fx.svc.ProcessNext(context.Background()); err != nil || fx.jobs.failed[qid(2)] != "internal_error" {
		t.Errorf("unclamped: err=%v failed=%v", err, fx.jobs.failed)
	}

	fx = newExportFixture(t)
	fx.jobs.claim = clampedJob(qid(3), from, to)
	fx.jobs.doneErr = errors.New("commit lost")
	if worked, err := fx.svc.ProcessNext(context.Background()); !worked || !errors.Is(err, fx.jobs.doneErr) {
		t.Errorf("complete error must be returned: worked=%v err=%v", worked, err)
	}
}

// The worker heartbeats the lease while producing (lease/3 cadence).
func TestExport_HeartbeatRenewsLease(t *testing.T) {
	fx := newExportFixture(t)
	fx.svc.cfg.Lease = 30 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { fx.svc.heartbeat(ctx, domain.ExportJob{ID: qid(1), TenantID: qTenant}); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		fx.jobs.mu.Lock()
		n := fx.jobs.beats
		fx.jobs.mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if fx.jobs.beats < 2 {
		t.Errorf("beats = %d", fx.jobs.beats)
	}
}

// Run drains claimable jobs, then returns once ctx is cancelled.
func TestExport_RunProcessesThenExitsOnCancel(t *testing.T) {
	fx := newExportFixture(t)
	fx.svc.cfg.PollInterval = 5 * time.Millisecond
	fx.jobs.claim = clampedJob(qid(1), qNow.AddDate(0, -1, 0), qNow)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { fx.svc.Run(ctx); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		fx.jobs.mu.Lock()
		claimed := fx.jobs.claim == nil
		fx.jobs.mu.Unlock()
		if claimed || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	fx.jobs.mu.Lock()
	fx.jobs.claimErr = errors.New("db blip") // logged, loop continues
	fx.jobs.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestNewExportService_Defaults(t *testing.T) {
	svc := NewExportService(newFakeJobs(), &fakeReader{}, &fakeArchive{}, &fakeExportStore{}, nil, nil, ExportConfig{}, nil)
	if svc.cfg.PollInterval != 5*time.Second || svc.cfg.Lease != 15*time.Minute || svc.cfg.Now == nil {
		t.Errorf("defaults = %+v", svc.cfg)
	}
}
