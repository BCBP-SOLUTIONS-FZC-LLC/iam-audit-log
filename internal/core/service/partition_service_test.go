package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
)

type fakePartitions struct {
	results         []domain.PartitionResult
	err             error
	ahead, trailing int
}

func (f *fakePartitions) EnsurePartitions(_ context.Context, ahead, trailing int) ([]domain.PartitionResult, error) {
	f.ahead, f.trailing = ahead, trailing
	return f.results, f.err
}

type recLog struct{ warns, infos int }

func (r *recLog) Debug(string, map[string]any) {}
func (r *recLog) Info(string, map[string]any)  { r.infos++ }
func (r *recLog) Warn(string, map[string]any)  { r.warns++ }
func (r *recLog) Error(string, map[string]any) {}

func TestNewPartitionService_ValidatesWindow(t *testing.T) {
	for _, tc := range []struct{ ahead, trailing int }{{-1, 3}, {3, -1}, {25, 3}, {3, 25}} {
		if _, err := NewPartitionService(&fakePartitions{}, nil, tc.ahead, tc.trailing); err == nil {
			t.Errorf("ahead=%d trailing=%d must be rejected", tc.ahead, tc.trailing)
		}
	}
	if _, err := NewPartitionService(&fakePartitions{}, nil, 0, 24); err != nil {
		t.Errorf("bounds are inclusive: %v", err)
	}
}

// LLD §4.4 / RB-3: outcomes are counted; a month blocked by default-
// partition rows is reported and warned, not an error.
func TestEnsureAhead_SummarisesAndWarnsOnSkipped(t *testing.T) {
	now := time.Now()
	f := &fakePartitions{results: []domain.PartitionResult{
		{Name: "audit_events_2026_09", Action: domain.PartitionExists, RangeStart: now, RangeEnd: now},
		{Name: "audit_events_2026_10", Action: domain.PartitionCreated},
		{Name: "audit_events_2026_11", Action: domain.PartitionCreated},
		{Name: "audit_events_2026_12", Action: domain.PartitionSkippedDefaultHasRows},
	}}
	log := &recLog{}
	svc, err := NewPartitionService(f, log, 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := svc.EnsureAhead(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Created != 2 || sum.Existing != 1 || len(sum.Skipped) != 1 || sum.Skipped[0] != "audit_events_2026_12" {
		t.Errorf("summary = %+v", sum)
	}
	if f.ahead != 3 || f.trailing != 2 {
		t.Errorf("window passed = %d/%d", f.ahead, f.trailing)
	}
	if log.warns != 1 || log.infos != 1 {
		t.Errorf("warns=%d infos=%d", log.warns, log.infos)
	}
}

func TestEnsureAhead_PropagatesManagerError(t *testing.T) {
	boom := errors.New("db down")
	svc, _ := NewPartitionService(&fakePartitions{err: boom}, nil, 3, 3)
	if _, err := svc.EnsureAhead(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestEnsureAhead_NilLoggerIsSafe(t *testing.T) {
	svc, _ := NewPartitionService(&fakePartitions{results: []domain.PartitionResult{{Action: domain.PartitionSkippedDefaultHasRows}}}, nil, 1, 1)
	if _, err := svc.EnsureAhead(context.Background()); err != nil {
		t.Fatal(err)
	}
}
