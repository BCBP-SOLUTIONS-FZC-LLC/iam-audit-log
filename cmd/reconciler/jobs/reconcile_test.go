package jobs

import (
	"context"
	"errors"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
)

type fakePartitions struct {
	res []domain.PartitionResult
	err error
}

func (f fakePartitions) EnsurePartitions(context.Context, int, int) ([]domain.PartitionResult, error) {
	return f.res, f.err
}

func TestReconcile_PartitionStep(t *testing.T) {
	jctx := &Context{
		Partitions: fakePartitions{res: []domain.PartitionResult{
			{Action: domain.PartitionCreated}, {Action: domain.PartitionExists},
			{Name: "audit_events_2027_01", Action: domain.PartitionSkippedDefaultHasRows},
		}},
		PrecreateMonths: 3, WritableTrailingMonths: 3,
	}
	res, err := Reconcile(context.Background(), jctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Attempted != 3 || res.Succeeded != 2 || res.Skipped != 1 {
		t.Errorf("result = %+v", res)
	}
}

func TestReconcile_Errors(t *testing.T) {
	if _, err := Reconcile(context.Background(), &Context{}); err == nil {
		t.Error("missing PartitionManager must fail")
	}
	if _, err := Reconcile(context.Background(), &Context{Partitions: fakePartitions{}, PrecreateMonths: 99}); err == nil {
		t.Error("invalid window must fail")
	}
	boom := errors.New("db down")
	if _, err := Reconcile(context.Background(), &Context{Partitions: fakePartitions{err: boom}, PrecreateMonths: 3}); !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
}
