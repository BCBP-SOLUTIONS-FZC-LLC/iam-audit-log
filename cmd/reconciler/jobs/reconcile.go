package jobs

import (
	"context"
	"fmt"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/service"
)

// Reconcile is the RECONCILER_SCHEDULE CronJob (LLD §8.5, §8.6, §12):
//  1. pre-create monthly partitions (AUDIT_PRECREATE_MONTHS ahead,
//     AUDIT_WRITABLE_TRAILING_MONTHS behind; §4.4);
//  2. archival: re-open dropped months that received late rows (D-19), then
//     for every month past the hot window archive and verify each retained
//     tier and drop the partition only through the AL-INV-9 gate, skipping
//     any partition touched by a pending redaction (AL-INV-12).
//
// A partition that could not be dropped is counted (Result.Failed), and the
// run then exits non-zero so the CronJob surfaces it.
func Reconcile(ctx context.Context, jctx *Context) (Result, error) {
	var res Result
	if jctx.Partitions == nil {
		return res, fmt.Errorf("reconcile: no PartitionManager wired")
	}
	svc, err := service.NewPartitionService(jctx.Partitions, jctx.RawLogger, jctx.PrecreateMonths, jctx.WritableTrailingMonths)
	if err != nil {
		return res, err
	}
	sum, err := svc.EnsureAhead(ctx)
	if err != nil {
		return res, err
	}
	res.Attempted = sum.Created + sum.Existing + len(sum.Skipped)
	res.Succeeded = sum.Created + sum.Existing
	res.Skipped = len(sum.Skipped)

	if jctx.Archives == nil || jctx.ArchiveStore == nil {
		return res, fmt.Errorf("reconcile: archival not wired")
	}
	arch := service.NewArchiveService(jctx.Archives, jctx.ArchiveStore, jctx.ArchiveMetrics, service.ArchiveConfig{
		HotWindowDays: jctx.HotWindowDays, WritableTrailingMonths: jctx.WritableTrailingMonths,
		PartMaxRows: jctx.ArchivePartMaxRows, WorkDir: jctx.ArchiveWorkDir,
	}, jctx.RawLogger)
	as, err := arch.Run(ctx)
	if err != nil {
		return res, fmt.Errorf("reconcile: archival: %w", err)
	}
	jctx.Logger.InfoContext(ctx, "archival pass complete", "reopened", as.Reopened, "dropped", as.Dropped,
		"blocked", as.Blocked, "stalled", as.Stalled, "not_eligible", as.Skipped)
	res.Attempted += as.Dropped + as.Blocked + as.Stalled
	res.Succeeded += as.Dropped
	res.Failed += as.Blocked + as.Stalled
	if res.Failed > 0 {
		return res, fmt.Errorf("reconcile: %d partition(s) blocked or stalled (AL-INV-9/12 held; see logs)", res.Failed)
	}
	return res, nil
}

// ProcessedEventsPrune is the PROCESSED_EVENTS_PRUNE_SCHEDULE CronJob (LLD
// §4.2): delete ledger rows older than PROCESSED_EVENTS_TTL_DAYS (> the
// 7-day SQS lifetime); uq_audit_events_source_id keeps dedup beyond it
// (AL-INV-4).
func ProcessedEventsPrune(ctx context.Context, jctx *Context) (Result, error) {
	var res Result
	if jctx.Ledger == nil {
		return res, fmt.Errorf("processed-events-prune: no LedgerPruner wired")
	}
	n, err := jctx.Ledger.PruneProcessedEvents(ctx, jctx.ProcessedEventsTTLDays, jctx.ProcessedEventsBatch)
	res.Attempted, res.Succeeded = int(n), int(n)
	if err != nil {
		return res, fmt.Errorf("processed-events-prune: %w", err)
	}
	jctx.Logger.InfoContext(ctx, "processed_events pruned", "rows", n, "ttl_days", jctx.ProcessedEventsTTLDays)
	return res, nil
}
