package domain

import "time"

// OpsStatsQuery parameterises audit_ops_stats() with the same archival
// schedule the reconciler uses (AUDIT_HOT_WINDOW_DAYS,
// AUDIT_WRITABLE_TRAILING_MONTHS), so "eligible" means the same thing in
// both places (ArchiveEligible).
type OpsStatsQuery struct {
	HotWindowDays          int
	WritableTrailingMonths int
	// StallGrace is how long past eligibility a partition may still exist
	// before it counts as stalled (the nightly run needs time).
	StallGrace time.Duration
	// PendingAge is how old a pending redaction task must be to count as
	// stuck (the immediate apply normally finishes in milliseconds).
	PendingAge time.Duration
}

// OpsStats is the DB-derived operational state exported by cmd/server as
// gauges (LLD §11; decision D-21, gap 40/41).
type OpsStats struct {
	DefaultPartitionRows int64   // rows in audit_events_default (RB-3)
	StalledPartitions    int64   // eligible past StallGrace and still attached (AL-INV-9, RB-2)
	ArchiveLagSeconds    float64 // age past eligibility of the oldest still-attached partition (0 if none)
	PendingRedactions    int64   // pending tasks older than PendingAge (RB-7)
}
