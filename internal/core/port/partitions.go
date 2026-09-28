package port

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
)

// PartitionManager creates missing monthly audit_events partitions for a
// window of months around the current one (LLD §4.4).
type PartitionManager interface {
	EnsurePartitions(ctx context.Context, ahead, trailing int) ([]domain.PartitionResult, error)
}
