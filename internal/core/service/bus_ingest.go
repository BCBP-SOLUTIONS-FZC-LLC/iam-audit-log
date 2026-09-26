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
	stored, created, err := s.store.Append(ctx, entry, consumer)
	if err != nil {
		return BusResult{}, err
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
