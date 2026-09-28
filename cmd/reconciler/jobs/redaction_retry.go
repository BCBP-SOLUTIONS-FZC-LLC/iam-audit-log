package jobs

import (
	"context"
	"fmt"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
)

// RedactionRetry is the REDACTION_RETRY_SCHEDULE CronJob (LLD §8.7, RB-7):
// it re-applies redaction tasks left pending because the immediate apply in
// cmd/server failed. apply_redaction() is idempotent and decides
// applied / not_applicable / missed itself (D-17). A task that fails again
// stays pending for the next run, and the run reports failure.
func RedactionRetry(ctx context.Context, jctx *Context) (Result, error) {
	var res Result
	if jctx.Redactions == nil {
		return res, fmt.Errorf("redaction-retry: no RedactionTasks wired")
	}
	ids, err := jctx.Redactions.PendingTasks(ctx, time.Now().Add(-jctx.RedactionRetryMinAge), jctx.RedactionRetryBatch)
	if err != nil {
		return res, fmt.Errorf("redaction-retry: list pending: %w", err)
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		res.Attempted++
		out, err := jctx.Redactions.ApplyRedaction(ctx, id)
		if err != nil {
			res.Failed++
			jctx.count(domain.RedactionPending)
			jctx.Logger.ErrorContext(ctx, "redaction retry failed — task stays pending (RB-7)", "redaction_task_id", id, "error", err.Error())
			continue
		}
		res.Succeeded++
		jctx.count(out.Status)
		// missed is routine (AL-Q15, Option A): archived rows are retained.
		jctx.Logger.InfoContext(ctx, "redaction retried", "redaction_task_id", id, "status", out.Status, "rows_redacted", out.RowsRedacted)
	}
	if res.Failed > 0 {
		return res, fmt.Errorf("redaction-retry: %d of %d tasks still failing", res.Failed, res.Attempted)
	}
	return res, nil
}

func (c *Context) count(status string) {
	if c.RedactionMetrics != nil {
		c.RedactionMetrics.TaskOutcome(status)
	}
}

// RedactionSweep is the REDACTION_SWEEP_SCHEDULE CronJob (daily; decision
// D-18, gap 37): defense in depth behind the ingest-time check. It
// re-redacts any unredacted security_3y row of a subject whose redaction
// finished within REDACTION_SWEEP_WINDOW. A non-zero count means a row
// slipped past the ingest check (a race with a concurrent apply, a bug, or a
// manual backfill) and is logged at warn.
func RedactionSweep(ctx context.Context, jctx *Context) (Result, error) {
	var res Result
	if jctx.Redactions == nil {
		return res, fmt.Errorf("redaction-sweep: no RedactionTasks wired")
	}
	n, err := jctx.Redactions.Sweep(ctx, jctx.RedactionSweepWindow)
	if err != nil {
		return res, fmt.Errorf("redaction-sweep: %w", err)
	}
	res.Attempted, res.Succeeded = int(n), int(n)
	if n > 0 {
		jctx.Logger.WarnContext(ctx, "redaction sweep fixed rows that bypassed the ingest-time check (D-18)", "rows", n)
	} else {
		jctx.Logger.InfoContext(ctx, "redaction sweep: nothing to fix")
	}
	return res, nil
}
