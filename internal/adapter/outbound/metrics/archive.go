package metrics

import coredomain "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"

// Archive implements port.ArchiveMetrics (LLD §11).
type Archive struct{}

// PartitionArchived counts one tier's archival outcome.
func (Archive) PartitionArchived(tier coredomain.RetentionTier, result string) {
	ArchivePartitions.WithLabelValues(string(tier), result).Inc()
}

// RedactionBlocked counts an archival refused for a pending redaction task.
func (Archive) RedactionBlocked() { RedactionBlockedArchive.Inc() }

// Pruned counts a partition drop for one tier.
func (Archive) Pruned(tier coredomain.RetentionTier) {
	RetentionPruned.WithLabelValues(string(tier)).Inc()
}

// Stalled sets the number of partitions the run could not drop.
func (Archive) Stalled(n int) { ArchiveStalled.Set(float64(n)) }
