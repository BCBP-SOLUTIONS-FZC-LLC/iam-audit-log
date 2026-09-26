// Package jobs holds the reconciler job bodies dispatched by --job
// (LLD §8.5, §8.6, §15.4): archival + verify + partition drop, partition
// pre-creation, and processed_events pruning. Every job runs on the
// audit_reconciler (BYPASSRLS) pool exclusively. Jobs land in Phase 1/7.
package jobs

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

// Context is the dependency bag every job accepts.
type Context struct {
	// Pool is the audit_reconciler pool — the only pool this binary opens.
	Pool   *pgcommon.Pool
	Logger port.SlogStyleLogger
	// RawLogger is the structured logger core services take.
	RawLogger port.Logger

	// Partitions creates monthly audit_events partitions (LLD §4.4).
	Partitions port.PartitionManager
	// Redactions lists and re-applies stuck redaction tasks (LLD §8.7).
	Redactions port.RedactionTasks
	// RedactionMetrics counts outcomes (collectors exist; a CronJob has no
	// scrape endpoint until Phase 8's push path).
	RedactionMetrics port.RedactionMetrics

	HotWindowDays          int
	PrecreateMonths        int
	WritableTrailingMonths int
	ProcessedEventsTTLDays int
	RedactionRetryMinAge   time.Duration
	RedactionRetryBatch    int
	RedactionSweepWindow   time.Duration
}

// Result summarizes a job run.
type Result struct {
	Attempted int
	Succeeded int
	Failed    int
	Skipped   int
}

// Func is the signature every job satisfies.
type Func func(ctx context.Context, jctx *Context) (Result, error)

// Registry maps --job names to job bodies (LLD §12 RECONCILER_SCHEDULE /
// PROCESSED_EVENTS_PRUNE_SCHEDULE). processed-events-prune lands in Phase 7.
func Registry() map[string]Func {
	return map[string]Func{
		"reconcile":       Reconcile,
		"redaction-retry": RedactionRetry,
		"redaction-sweep": RedactionSweep,
	}
}
