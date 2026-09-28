package port

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
)

// AuditWriter persists audit entries. Append is the ONE write path every
// transport converges on (AL-INV-2): in a single transaction bound to
// entry.TenantID (so RLS WITH CHECK guards the row, AL-INV-3) it records
// (entry.SourceEventID, consumer) in the processed_events ledger and
// inserts the row. A duplicate — a ledger hit, or the
// (source_event_id, occurred_at) backstop — is a no-op that returns the
// already-persisted row with created=false (AL-INV-4).
//
// If the key is already recorded but no row for it is visible under
// entry.TenantID (the key was used for another tenant), Append returns a
// domain invalid_request error: a replay must never reveal or alias
// another tenant's entry.
type AuditWriter interface {
	Append(ctx context.Context, entry domain.AuditEntry, consumer string) (stored domain.AuditEntry, created bool, err error)
}

// IDGenerator mints audit_events.id values (UUIDv7, LLD §4.2).
type IDGenerator func() (string, error)

// Clock returns the current time.
type Clock func() time.Time

// PlanProjector maintains the tenant_plan_window projection (LLD §4.2): an
// upsert guarded by last_event_at so an out-of-order or replayed event never
// regresses a tenant's plan (AL-EVT-3). applied=false means the guard
// rejected a stale event.
type PlanProjector interface {
	ProjectPlan(ctx context.Context, tenantID, planCode string, queryWindowDays int, eventAt time.Time) (applied bool, err error)
}

// PlanWindows resolves plan_code → query_window_days (the CAT-I2 map,
// AL-D15 — implemented by the Phase 5 poller).
type PlanWindows interface {
	WindowDays(planCode string) (int, bool)
}

// RedactionStore is the GDPR redaction side of the write path (LLD §8.7;
// decisions D-1, D-15..D-17).
type RedactionStore interface {
	// AppendWithRedaction is AuditWriter.Append plus, in the same
	// transaction, an audit_redaction_tasks insert that is a no-op on a
	// redelivered trigger. taskCreated reports whether req's task was
	// inserted, so it should be applied now.
	AppendWithRedaction(ctx context.Context, entry domain.AuditEntry, consumer string, req domain.RedactionRequest) (stored domain.AuditEntry, created, taskCreated bool, err error)
	// ApplyRedaction runs apply_redaction(taskID). It is idempotent: a
	// finished task only reports its outcome.
	ApplyRedaction(ctx context.Context, taskID string) (domain.RedactionOutcome, error)
}

// RedactionTasks lists stuck tasks for the reconciler's retry job.
type RedactionTasks interface {
	// PendingTasks returns up to limit pending task ids requested before
	// olderThan, oldest first.
	PendingTasks(ctx context.Context, olderThan time.Time, limit int) ([]string, error)
	ApplyRedaction(ctx context.Context, taskID string) (domain.RedactionOutcome, error)
	// Sweep re-redacts unredacted security_3y rows of subjects whose
	// redaction finished within window (sweep_redactions(), D-18) and
	// returns the rows fixed.
	Sweep(ctx context.Context, window time.Duration) (int64, error)
}

// RedactionMetrics counts task outcomes (iam_audit_log_redaction_tasks_total).
type RedactionMetrics interface {
	TaskOutcome(status string)
}
