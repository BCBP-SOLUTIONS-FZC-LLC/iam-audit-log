package domain

import "time"

// PartitionAction is what audit_ensure_partitions() did for one month.
type PartitionAction string

// PartitionAction values (migration 000005).
const (
	PartitionCreated PartitionAction = "created"
	PartitionExists  PartitionAction = "exists"
	// PartitionSkippedDefaultHasRows: rows for that month already sit in
	// audit_events_default, so PostgreSQL refuses the new bound. The rows
	// must be re-homed first (RB-3).
	PartitionSkippedDefaultHasRows PartitionAction = "skipped_default_has_rows"
)

// MaxPartitionWindow bounds each side of the pre-creation window; it
// mirrors the guard inside audit_ensure_partitions().
const MaxPartitionWindow = 24

// PartitionResult is one monthly partition's outcome.
type PartitionResult struct {
	Name       string // audit_events_YYYY_MM
	RangeStart time.Time
	RangeEnd   time.Time
	Action     PartitionAction
}

// PartitionName returns the monthly partition name for t (UTC month),
// matching audit_ensure_partitions(): audit_events_YYYY_MM (LLD §4.2).
func PartitionName(t time.Time) string {
	return "audit_events_" + t.UTC().Format("2006_01")
}
