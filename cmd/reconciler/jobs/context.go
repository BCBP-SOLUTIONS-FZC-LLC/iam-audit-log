// Package jobs holds the reconciler job bodies dispatched by --job
// (LLD §8.5, §8.6, §15.4): archival + verify + partition drop, partition
// pre-creation, processed_events pruning, and the redaction retry/sweep.
// Every job runs on the audit_reconciler (BYPASSRLS) pool exclusively.
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
	// Archives / ArchiveStore / ArchiveMetrics drive archival (LLD §8.5).
	Archives       port.ArchiveRepository
	ArchiveStore   port.ArchiveStore
	ArchiveMetrics port.ArchiveMetrics
	// Ledger prunes processed_events (LLD §4.2).
	Ledger port.LedgerPruner

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
	ArchivePartMaxRows     int
	ArchiveWorkDir         string
	ProcessedEventsBatch   int
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
// PROCESSED_EVENTS_PRUNE_SCHEDULE / the redaction schedules).
func Registry() map[string]Func {
	return map[string]Func{
		"reconcile":              Reconcile,
		"redaction-retry":        RedactionRetry,
		"redaction-sweep":        RedactionSweep,
		"processed-events-prune": ProcessedEventsPrune,
	}
}
