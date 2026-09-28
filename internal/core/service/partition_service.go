// Package service holds the use cases (LLD §3): IngestService, QueryService,
// ExportService, ArchiveService, RetentionService, PartitionService,
// RedactionService. Imports core/domain + core/port only.
package service

import (
	"context"
	"fmt"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
)

// PartitionService keeps monthly audit_events partitions created ahead of
// need (LLD §4.2, §4.4, AL-D4): AUDIT_PRECREATE_MONTHS ahead plus
// AUDIT_WRITABLE_TRAILING_MONTHS behind, so late arrivals still route to a
// real partition. It runs at server startup (defensively) and from the
// reconciler CronJob. Partition management is runtime, not migration.
type PartitionService struct {
	mgr      port.PartitionManager
	log      port.Logger
	ahead    int
	trailing int
}

// PartitionSummary counts one EnsureAhead run's outcomes.
type PartitionSummary struct {
	Created  int
	Existing int
	Skipped  []string // months blocked by rows in audit_events_default (RB-3)
}

// NewPartitionService validates the window (each side 0..24 months).
func NewPartitionService(mgr port.PartitionManager, log port.Logger, ahead, trailing int) (*PartitionService, error) {
	for name, v := range map[string]int{"AUDIT_PRECREATE_MONTHS": ahead, "AUDIT_WRITABLE_TRAILING_MONTHS": trailing} {
		if v < 0 || v > domain.MaxPartitionWindow {
			return nil, fmt.Errorf("%s=%d out of range 0..%d", name, v, domain.MaxPartitionWindow)
		}
	}
	return &PartitionService{mgr: mgr, log: log, ahead: ahead, trailing: trailing}, nil
}

// EnsureAhead creates any missing partition in the window. A month blocked
// by default-partition rows is reported and logged (RB-3), not an error —
// the DEFAULT partition keeps ingestion working meanwhile.
func (s *PartitionService) EnsureAhead(ctx context.Context) (PartitionSummary, error) {
	var sum PartitionSummary
	results, err := s.mgr.EnsurePartitions(ctx, s.ahead, s.trailing)
	if err != nil {
		return sum, fmt.Errorf("ensure partitions: %w", err)
	}
	for _, r := range results {
		switch r.Action {
		case domain.PartitionCreated:
			sum.Created++
		case domain.PartitionExists:
			sum.Existing++
		case domain.PartitionSkippedDefaultHasRows:
			sum.Skipped = append(sum.Skipped, r.Name)
		}
	}
	if s.log != nil {
		if len(sum.Skipped) > 0 {
			s.log.Warn("partition creation blocked by rows in audit_events_default — follow RB-3",
				map[string]any{"partitions": sum.Skipped})
		}
		s.log.Info("partitions ensured", map[string]any{
			"created": sum.Created, "existing": sum.Existing, "skipped": len(sum.Skipped),
			"ahead": s.ahead, "trailing": s.trailing,
		})
	}
	return sum, nil
}
