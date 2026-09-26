package http

import (
	"encoding/json"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
)

// ActorDTO is the §5.4 actor object.
type ActorDTO struct {
	Type    string `json:"type" example:"user" enums:"user,service_account,iam_system,anonymous"`
	ID      string `json:"id,omitempty" example:"7f3c2d1e-9a8b-4c5d-8e7f-0a1b2c3d4e5f"`
	Display string `json:"display,omitempty" example:"asha@acme.example"`
}

// TargetDTO is the §5.4 target object (omit for tenant-wide actions).
type TargetDTO struct {
	Type string `json:"type" example:"tenant_setting"`
	ID   string `json:"id,omitempty" example:"mfa_freshness_seconds"`
}

// DirectWriteRequest is the AL-5 body (LLD §5.4). There is no tier field:
// a caller-supplied "retention_tier" is ignored and the tier is derived
// from entry_type (AL-INV-6, AL-INV-11).
type DirectWriteRequest struct {
	TenantID        string          `json:"tenant_id" example:"00000000-0000-0000-0000-0000000000b1"`
	EntryType       string          `json:"entry_type" example:"config.tenant_setting.changed"`
	Action          string          `json:"action" example:"update"`
	Actor           ActorDTO        `json:"actor"`
	Target          *TargetDTO      `json:"target,omitempty"`
	OccurredAt      time.Time       `json:"occurred_at" example:"2026-03-14T09:12:04Z"`
	SourceService   string          `json:"source_service" example:"iam-org-membership"`
	SourceEventType string          `json:"source_event_type" example:"TenantSettingChanged"`
	IPAddress       string          `json:"ip_address,omitempty" example:"203.0.113.7"`
	UserAgent       string          `json:"user_agent,omitempty"`
	TraceID         string          `json:"trace_id,omitempty"`
	Metadata        json.RawMessage `json:"metadata,omitempty" swaggertype:"object"`
}

// BatchEntryRequest is one AL-6 entry: the AL-5 body plus its own
// idempotency_key (decision D-4).
type BatchEntryRequest struct {
	DirectWriteRequest
	IdempotencyKey string `json:"idempotency_key" example:"4b1f0f8e-2c1d-5e3a-9f0b-7c6d5e4f3a21"`
}

// BatchRequest is the AL-6 body.
type BatchRequest struct {
	Entries []BatchEntryRequest `json:"entries"`
}

// EntryResponse is a persisted audit entry.
type EntryResponse struct {
	ID              string          `json:"id"`
	TenantID        string          `json:"tenant_id"`
	OccurredAt      time.Time       `json:"occurred_at"`
	RecordedAt      time.Time       `json:"recorded_at"`
	EntryType       string          `json:"entry_type"`
	Action          string          `json:"action"`
	Actor           ActorDTO        `json:"actor"`
	Target          *TargetDTO      `json:"target,omitempty"`
	SourceService   string          `json:"source_service"`
	SourceTopic     *string         `json:"source_topic"`
	SourceEventType string          `json:"source_event_type"`
	SourceEventID   string          `json:"source_event_id"`
	RetentionTier   string          `json:"retention_tier"`
	IngestMode      string          `json:"ingest_mode"`
	IPAddress       string          `json:"ip_address,omitempty"`
	UserAgent       string          `json:"user_agent,omitempty"`
	TraceID         string          `json:"trace_id,omitempty"`
	Metadata        json.RawMessage `json:"metadata" swaggertype:"object"`
}

// BatchResultItem is one AL-6 per-index outcome (decision D-4).
type BatchResultItem struct {
	Index   int    `json:"index"`
	Status  int    `json:"status" example:"201"`
	ID      string `json:"id,omitempty"`
	Code    string `json:"code,omitempty" example:"unknown_entry_type"`
	Message string `json:"message,omitempty"`
}

// BatchResponse is the AL-6 207 body.
type BatchResponse struct {
	Results []BatchResultItem `json:"results"`
}

func (r DirectWriteRequest) command(key, defaultTraceID string) domain.DirectWriteCommand {
	cmd := domain.DirectWriteCommand{
		IdempotencyKey:  key,
		TenantID:        r.TenantID,
		EntryType:       r.EntryType,
		Action:          r.Action,
		Actor:           domain.ActorRef{Type: domain.ActorType(r.Actor.Type), ID: r.Actor.ID, Display: r.Actor.Display},
		OccurredAt:      r.OccurredAt,
		SourceService:   r.SourceService,
		SourceEventType: r.SourceEventType,
		IPAddress:       r.IPAddress,
		UserAgent:       r.UserAgent,
		TraceID:         r.TraceID,
		Metadata:        r.Metadata,
	}
	if cmd.TraceID == "" {
		cmd.TraceID = defaultTraceID // taken from the current W3C traceparent (§5.4)
	}
	if r.Target != nil {
		cmd.Target = &domain.TargetRef{Type: r.Target.Type, ID: r.Target.ID}
	}
	return cmd
}

func toEntryResponse(e domain.AuditEntry) EntryResponse {
	out := EntryResponse{
		ID: e.ID, TenantID: e.TenantID, OccurredAt: e.OccurredAt, RecordedAt: e.RecordedAt,
		EntryType: e.EntryType, Action: e.Action,
		Actor:         ActorDTO{Type: string(e.Actor.Type), ID: e.Actor.ID, Display: e.Actor.Display},
		SourceService: e.SourceService, SourceEventType: e.SourceEventType, SourceEventID: e.SourceEventID,
		RetentionTier: string(e.RetentionTier), IngestMode: string(e.IngestMode),
		IPAddress: e.IPAddress, UserAgent: e.UserAgent, TraceID: e.TraceID, Metadata: e.Metadata,
	}
	if e.SourceTopic != "" {
		t := e.SourceTopic
		out.SourceTopic = &t
	}
	if e.Target != nil {
		out.Target = &TargetDTO{Type: e.Target.Type, ID: e.Target.ID}
	}
	if len(out.Metadata) == 0 {
		out.Metadata = json.RawMessage(`{}`)
	}
	return out
}
