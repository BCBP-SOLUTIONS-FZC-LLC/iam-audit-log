package jobs

import (
	"context"
	"fmt"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/service"
)

// Reconcile is the RECONCILER_SCHEDULE CronJob (LLD §8.5, §8.6, §12).
// Step 1 (Phase 1): pre-create monthly partitions — AUDIT_PRECREATE_MONTHS
// ahead and AUDIT_WRITABLE_TRAILING_MONTHS behind (§4.4). Archival, checksum
// verification, the redaction-pending refusal and the provable partition
// drop (AL-INV-9, AL-INV-12) are added in Phase 7.
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
	return res, nil
}
