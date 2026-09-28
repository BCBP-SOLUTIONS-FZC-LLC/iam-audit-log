package metrics

import (
	"context"
	"errors"
	"io"
	"time"

	coredomain "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
)

// Dependency implements port.DependencyObserver
// (platform_dependency_request_seconds{dependency,operation,outcome}).
type Dependency struct{}

// ObserveDependency times one dependency call.
func (Dependency) ObserveDependency(dependency, operation, outcome string, took time.Duration) {
	DependencyRequestSeconds.WithLabelValues(dependency, operation, outcome).Observe(max(took, 0).Seconds())
}

// dependencyOutcome maps a call's error to the registry's outcome vocabulary.
func dependencyOutcome(err error) string {
	switch {
	case err == nil:
		return port.OutcomeSuccess
	case errors.Is(err, port.ErrObjectMissing):
		return port.OutcomeNotFound
	case errors.Is(err, context.DeadlineExceeded):
		return port.OutcomeTimeout
	default:
		return port.OutcomeError
	}
}

func observeS3(operation string, start time.Time, err error) {
	Dependency{}.ObserveDependency(port.DependencyS3, operation, dependencyOutcome(err), time.Since(start))
}

// InstrumentedArchiveReader times ArchiveReader calls against S3.
type InstrumentedArchiveReader struct{ Inner port.ArchiveReader }

// ReadArchive implements port.ArchiveReader.
func (i InstrumentedArchiveReader) ReadArchive(ctx context.Context, bucket, key string, fn func(coredomain.ArchiveRecord) error) error {
	start := time.Now()
	err := i.Inner.ReadArchive(ctx, bucket, key, fn)
	observeS3(port.OperationReadArchive, start, err)
	return err
}

// InstrumentedExportStore times ExportStore uploads. Presigning is local
// signing, not a network call, so it is not a dependency request.
type InstrumentedExportStore struct{ Inner port.ExportStore }

// PutExport implements port.ExportStore.
func (i InstrumentedExportStore) PutExport(ctx context.Context, key string, body io.ReadSeeker, size int64) error {
	start := time.Now()
	err := i.Inner.PutExport(ctx, key, body, size)
	observeS3(port.OperationPutExport, start, err)
	return err
}

// PresignExport implements port.ExportStore.
func (i InstrumentedExportStore) PresignExport(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return i.Inner.PresignExport(ctx, key, ttl)
}

// InstrumentedArchiveStore times the reconciler's ArchiveStore calls.
type InstrumentedArchiveStore struct{ Inner port.ArchiveStore }

// PutArchive implements port.ArchiveStore.
func (i InstrumentedArchiveStore) PutArchive(ctx context.Context, key string, body io.ReadSeeker, size int64, retainUntil time.Time) error {
	start := time.Now()
	err := i.Inner.PutArchive(ctx, key, body, size, retainUntil)
	observeS3(port.OperationPutArchive, start, err)
	return err
}

// ArchiveChecksum implements port.ArchiveStore.
func (i InstrumentedArchiveStore) ArchiveChecksum(ctx context.Context, key string, retainUntil time.Time) (string, error) {
	start := time.Now()
	sum, err := i.Inner.ArchiveChecksum(ctx, key, retainUntil)
	observeS3(port.OperationVerifyArchive, start, err)
	return sum, err
}

// Bucket implements port.ArchiveStore.
func (i InstrumentedArchiveStore) Bucket() string { return i.Inner.Bucket() }

var (
	_ port.DependencyObserver = Dependency{}
	_ port.ArchiveReader      = InstrumentedArchiveReader{}
	_ port.ExportStore        = InstrumentedExportStore{}
	_ port.ArchiveStore       = InstrumentedArchiveStore{}
)
