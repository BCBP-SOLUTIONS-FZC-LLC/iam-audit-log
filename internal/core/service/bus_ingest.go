package service

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
)

// planEvents are the §4.2 events that set a tenant's plan: TenantCreated /
// TrialStarted (initial) and TenantConverted / TenantPlanChanged (tier
// change). Each carries it as payload field "plan" (producer survey);
// DirectPaidSignup carries none and TenantPlanChanged's is optional.
var planEvents = map[string]bool{
	"tenant.created": true, "tenant.trial_started": true,
	"tenant.converted": true, "tenant.plan_changed": true,
}

var planCodeShape = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// BusIngestConfig wires the bus path.
type BusIngestConfig struct {
	Plans             port.PlanProjector
	Windows           port.PlanWindows // nil until the Phase 5 poller
	DefaultWindowDays int              // AUDIT_DEFAULT_QUERY_WINDOW_DAYS
	Now               port.Clock
	// Redaction schedules and immediately applies GDPR redaction on
	// UserDeleted (LLD §8.7, AL-INV-12); nil disables it.
	Redaction        port.RedactionStore
	RedactionMetrics port.RedactionMetrics // may be nil
}

// WithBus enables IngestBus on the service.
func (s *IngestService) WithBus(cfg BusIngestConfig) *IngestService {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	s.bus = &cfg
	return s
}

// BusResult is IngestBus's outcome.
type BusResult struct {
	Entry   domain.AuditEntry
	Created bool // false: redelivery / replay (AL-INV-4)
	Known   bool // false: persisted as <domain>.unknown (AL-EVT-4)
}

// IngestBus normalises a consumed event and persists it through the same
// Append as the direct-write path (AL-INV-2). An unrecognized type is still
// persisted (AL-EVT-4); a plan-carrying tenant event also updates
// tenant_plan_window (recency-guarded, idempotent on redelivery).
func (s *IngestService) IngestBus(ctx context.Context, ev domain.BusEvent, consumer string) (BusResult, error) {
	if s.bus == nil {
		return BusResult{}, fmt.Errorf("ingest: bus path not configured")
	}
	entry, known, err := domain.BuildBusEntry(ev, s.maxMetadata, s.bus.Now())
	if err != nil {
		return BusResult{}, err
	}
	id, err := s.newID()
	if err != nil {
		return BusResult{}, fmt.Errorf("mint entry id: %w", err)
	}
	entry.ID = id
	stored, created, err := s.appendBus(ctx, entry, consumer)
	if err != nil {
		return BusResult{}, err
	}
	s.observe(stored, created, consumer)
	if !known && created && s.metrics != nil {
		s.metrics.Unknown(stored.SourceService)
	}
	if s.log != nil {
		fields := map[string]any{
			"entry_id": stored.ID, "entry_type": stored.EntryType, "source_event_type": ev.Type,
			"source_event_id": ev.ID, "tenant_id": stored.TenantID, "consumer": consumer, "created": created,
		}
		if !known {
			s.log.Warn("unrecognized event type persisted as "+stored.EntryType+" — extend the taxonomy (AL-EVT-4)", fields)
		} else {
			s.log.Info("bus event ingested", fields)
		}
	}
	// The projection runs on replays too: a crash between the audit commit
	// and this upsert is healed by the redelivery, and the recency guard
	// makes re-applying harmless.
	if err := s.projectPlan(ctx, entry, ev); err != nil {
		return BusResult{}, err
	}
	return BusResult{Entry: stored, Created: created, Known: known}, nil
}

// appendBus persists a bus entry. A UserDeleted also schedules its
// redaction task in the same transaction and, once committed, applies it
// immediately while the subject's recent rows are still hot (LLD §8.7).
// A failed apply leaves the task pending for the reconciler's
// redaction-retry job; it never fails the message, because the audit row
// and the task are already durable.
func (s *IngestService) appendBus(ctx context.Context, entry domain.AuditEntry, consumer string) (domain.AuditEntry, bool, error) {
	subject, trigger := domain.RedactionTrigger(entry)
	if !trigger || s.bus.Redaction == nil {
		return s.store.Append(ctx, entry, consumer)
	}
	if subject == "" {
		if s.log != nil {
			s.log.Error("UserDeleted names no usable user_id — no redaction task created (GDPR erasure not scheduled)", map[string]any{
				"source_event_id": entry.SourceEventID, "tenant_id": entry.TenantID,
			})
		}
		return s.store.Append(ctx, entry, consumer)
	}
	taskID, err := s.newID()
	if err != nil {
		return domain.AuditEntry{}, false, fmt.Errorf("mint redaction task id: %w", err)
	}
	stored, created, taskCreated, err := s.bus.Redaction.AppendWithRedaction(ctx, entry, consumer, domain.RedactionRequest{
		TaskID: taskID, TenantID: entry.TenantID, SubjectID: subject,
		TriggerEventType: domain.RedactionTriggerUserDeleted, TriggerSourceEventID: entry.SourceEventID,
	})
	if err != nil || !taskCreated {
		return stored, created, err
	}
	s.applyRedaction(ctx, taskID, entry.TenantID)
	return stored, created, nil
}

func (s *IngestService) applyRedaction(ctx context.Context, taskID, tenantID string) {
	out, err := s.bus.Redaction.ApplyRedaction(ctx, taskID)
	status := out.Status
	if err != nil {
		status = domain.RedactionPending
	}
	if s.bus.RedactionMetrics != nil {
		s.bus.RedactionMetrics.TaskOutcome(status)
	}
	if s.log == nil {
		return
	}
	fields := map[string]any{"redaction_task_id": taskID, "tenant_id": tenantID, "status": status, "rows_redacted": out.RowsRedacted}
	switch {
	case err != nil:
		fields["error"] = err.Error()
		s.log.Error("immediate redaction failed — task left pending for redaction-retry (RB-7)", fields)
	case status == domain.RedactionMissed:
		// Routine, not an alarm (AL-Q15, Option A): hot rows are redacted;
		// archived rows stay retained under Object Lock.
		s.log.Info("redaction applied to hot rows; the subject also has archived security_3y rows, which are retained (AL-Q15)", fields)
	default:
		s.log.Info("redaction applied", fields)
	}
}

func (s *IngestService) projectPlan(ctx context.Context, entry domain.AuditEntry, ev domain.BusEvent) error {
	if s.bus.Plans == nil || !planEvents[entry.EntryType] || ev.Time.IsZero() {
		return nil
	}
	var p struct {
		Plan string `json:"plan"`
	}
	if json.Unmarshal(ev.Payload, &p) != nil || !planCodeShape.MatchString(p.Plan) {
		return nil
	}
	days := s.bus.DefaultWindowDays
	if s.bus.Windows != nil {
		if d, ok := s.bus.Windows.WindowDays(p.Plan); ok {
			days = d
		}
	}
	if _, err := s.bus.Plans.ProjectPlan(ctx, entry.TenantID, p.Plan, days, ev.Time.UTC()); err != nil {
		return fmt.Errorf("project tenant plan window: %w", err)
	}
	return nil
}
