package metrics

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	coredomain "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
)

// labeledHistogram returns the sample count and sum of a histogram series
// matching every label pair (0, 0 if none).
func labeledHistogram(t *testing.T, name string, want map[string]string) (uint64, float64) {
	t.Helper()
	for _, m := range metricFamily(t, name).GetMetric() {
		match := 0
		for _, lp := range m.GetLabel() {
			if v, ok := want[lp.GetName()]; ok && v == lp.GetValue() {
				match++
			}
		}
		if match == len(want) {
			return m.GetHistogram().GetSampleCount(), m.GetHistogram().GetSampleSum()
		}
	}
	return 0, 0
}

func depCount(t *testing.T, dependency, operation, outcome string) uint64 {
	t.Helper()
	Dependency{}.ObserveDependency("probe", "probe", "probe", 0) // the family exists before any baseline read
	n, _ := labeledHistogram(t, "platform_dependency_request_seconds",
		map[string]string{"dependency": dependency, "operation": operation, "outcome": outcome})
	return n
}

// Outcome vocabulary of platform_dependency_request_seconds.
func TestDependencyOutcome(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"nil":       {nil, port.OutcomeSuccess},
		"missing":   {fmt.Errorf("get: %w", port.ErrObjectMissing), port.OutcomeNotFound},
		"deadline":  {fmt.Errorf("get: %w", context.DeadlineExceeded), port.OutcomeTimeout},
		"other":     {errors.New("boom"), port.OutcomeError},
		"cancelled": {context.Canceled, port.OutcomeError},
	} {
		if got := dependencyOutcome(tc.err); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}

func TestDependency_Observe(t *testing.T) {
	before := depCount(t, port.DependencyCatalogAdmin, port.OperationListPlans, port.OutcomeSuccess)
	Dependency{}.ObserveDependency(port.DependencyCatalogAdmin, port.OperationListPlans, port.OutcomeSuccess, 20*time.Millisecond)
	Dependency{}.ObserveDependency(port.DependencyCatalogAdmin, port.OperationListPlans, port.OutcomeSuccess, -time.Second) // clamped to 0
	if got := depCount(t, port.DependencyCatalogAdmin, port.OperationListPlans, port.OutcomeSuccess); got != before+2 {
		t.Errorf("count = %d, want %d", got, before+2)
	}
}

// The CAT-I2 poll adapter reports through the registry labels.
func TestCatalogPoll_DependencyLabels(t *testing.T) {
	before := depCount(t, "catalog-admin", "list_plans", "timeout")
	CatalogPoll{}.PollResult("timeout", time.Millisecond)
	if got := depCount(t, "catalog-admin", "list_plans", "timeout"); got != before+1 {
		t.Errorf("catalog timeout observations = %d, want %d", got, before+1)
	}
}

type fakeS3 struct {
	err      error
	sum      string
	calls    int
	presigns int
	bucket   string
}

func (f *fakeS3) ReadArchive(_ context.Context, _, _ string, fn func(coredomain.ArchiveRecord) error) error {
	f.calls++
	if f.err != nil {
		return f.err
	}
	return fn(coredomain.ArchiveRecord{ID: "x"})
}

func (f *fakeS3) PutExport(_ context.Context, _ string, _ io.ReadSeeker, _ int64) error {
	f.calls++
	return f.err
}

func (f *fakeS3) PresignExport(context.Context, string, time.Duration) (string, error) {
	f.presigns++
	return "https://signed", f.err
}

func (f *fakeS3) PutArchive(context.Context, string, io.ReadSeeker, int64, time.Time) error {
	f.calls++
	return f.err
}

func (f *fakeS3) ArchiveChecksum(context.Context, string, time.Time) (string, error) {
	f.calls++
	return f.sum, f.err
}

func (f *fakeS3) Bucket() string { return f.bucket }

// The S3 decorators pass every call through unchanged and time it as
// platform_dependency_request_seconds{dependency="s3"}.
func TestInstrumentedS3(t *testing.T) {
	ctx := context.Background()
	inner := &fakeS3{sum: "abc", bucket: "iam-audit-archive"}

	before := depCount(t, "s3", port.OperationReadArchive, port.OutcomeSuccess)
	var got []string
	if err := (InstrumentedArchiveReader{Inner: inner}).ReadArchive(ctx, "b", "k", func(r coredomain.ArchiveRecord) error {
		got = append(got, r.ID)
		return nil
	}); err != nil || len(got) != 1 {
		t.Fatalf("read passthrough: %v %v", got, err)
	}
	if depCount(t, "s3", port.OperationReadArchive, port.OutcomeSuccess) != before+1 {
		t.Error("read_archive not observed")
	}

	exp := InstrumentedExportStore{Inner: inner}
	before = depCount(t, "s3", port.OperationPutExport, port.OutcomeSuccess)
	if err := exp.PutExport(ctx, "k", bytes.NewReader(nil), 0); err != nil {
		t.Fatal(err)
	}
	if depCount(t, "s3", port.OperationPutExport, port.OutcomeSuccess) != before+1 {
		t.Error("put_export not observed")
	}
	if u, err := exp.PresignExport(ctx, "k", time.Minute); err != nil || u != "https://signed" || inner.presigns != 1 {
		t.Errorf("presign passthrough: %q %v", u, err)
	}

	arc := InstrumentedArchiveStore{Inner: inner}
	before = depCount(t, "s3", port.OperationPutArchive, port.OutcomeSuccess)
	if err := arc.PutArchive(ctx, "k", bytes.NewReader(nil), 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if depCount(t, "s3", port.OperationPutArchive, port.OutcomeSuccess) != before+1 {
		t.Error("put_archive not observed")
	}
	if sum, err := arc.ArchiveChecksum(ctx, "k", time.Now()); err != nil || sum != "abc" {
		t.Errorf("checksum passthrough: %q %v", sum, err)
	}
	if arc.Bucket() != "iam-audit-archive" {
		t.Error("bucket passthrough")
	}

	// Errors pass through unchanged and are classified.
	inner.err = fmt.Errorf("no such key: %w", port.ErrObjectMissing)
	before = depCount(t, "s3", port.OperationVerifyArchive, port.OutcomeNotFound)
	if _, err := arc.ArchiveChecksum(ctx, "k", time.Now()); !errors.Is(err, port.ErrObjectMissing) {
		t.Errorf("error passthrough: %v", err)
	}
	if depCount(t, "s3", port.OperationVerifyArchive, port.OutcomeNotFound) != before+1 {
		t.Error("not_found not observed")
	}
	inner.err = errors.New("s3 down")
	before = depCount(t, "s3", port.OperationReadArchive, port.OutcomeError)
	_ = (InstrumentedArchiveReader{Inner: inner}).ReadArchive(ctx, "b", "k", func(coredomain.ArchiveRecord) error { return nil })
	if depCount(t, "s3", port.OperationReadArchive, port.OutcomeError) != before+1 {
		t.Error("read error not observed")
	}
}
