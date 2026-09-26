package domain

import (
	"encoding/json"
	"fmt"
	"time"
)

// ArchiveObject is one archived S3 object's manifest row (decision D-10).
type ArchiveObject struct {
	Bucket        string
	Key           string
	TenantID      string
	Tier          RetentionTier
	PeriodMonth   time.Time
	Part          int
	RowCount      int64
	ByteSize      int64
	MinOccurredAt time.Time
	MaxOccurredAt time.Time
	// MinID/MaxID bound the object's row ids (UUIDv7, so ~recorded order):
	// an AL-2 lookup of an archived entry reads only the objects whose
	// range contains the id.
	MinID, MaxID string
}

// ArchiveKey is the D-10 object key: tier prefix (Object Lock / lifecycle
// per tier, §15.4), then tenant and month, so a tenant's archived month is
// read without scanning other tenants' rows.
func ArchiveKey(tier RetentionTier, tenantID string, month time.Time, part int) string {
	m := month.UTC()
	return fmt.Sprintf("%s/%s/%04d/%02d/audit_events_%04d_%02d-part-%04d.jsonl.gz",
		tier, tenantID, m.Year(), int(m.Month()), m.Year(), int(m.Month()), part)
}

// ExportKey is the export object key (LLD §25).
func ExportKey(tenantID, exportID string) string {
	return fmt.Sprintf("exports/%s/%s.jsonl.gz", tenantID, exportID)
}

// ArchiveRecord is one line of an archive or export object (gzipped JSONL):
// the full audit row, so an archived entry round-trips losslessly.
type ArchiveRecord struct {
	ID              string          `json:"id"`
	OccurredAt      time.Time       `json:"occurred_at"`
	RecordedAt      time.Time       `json:"recorded_at"`
	TenantID        string          `json:"tenant_id"`
	EntryType       string          `json:"entry_type"`
	Action          string          `json:"action"`
	ActorType       string          `json:"actor_type"`
	ActorID         string          `json:"actor_id,omitempty"`
	ActorDisplay    string          `json:"actor_display,omitempty"`
	TargetType      string          `json:"target_type,omitempty"`
	TargetID        string          `json:"target_id,omitempty"`
	SourceService   string          `json:"source_service"`
	SourceTopic     string          `json:"source_topic,omitempty"`
	SourceEventType string          `json:"source_event_type"`
	SourceEventID   string          `json:"source_event_id"`
	RetentionTier   string          `json:"retention_tier"`
	IngestMode      string          `json:"ingest_mode"`
	IPAddress       string          `json:"ip_address,omitempty"`
	UserAgent       string          `json:"user_agent,omitempty"`
	TraceID         string          `json:"trace_id,omitempty"`
	Metadata        json.RawMessage `json:"metadata"`
}

// ToRecord converts an entry to its archive line.
func ToRecord(e AuditEntry) ArchiveRecord {
	r := ArchiveRecord{
		ID: e.ID, OccurredAt: e.OccurredAt.UTC(), RecordedAt: e.RecordedAt.UTC(), TenantID: e.TenantID,
		EntryType: e.EntryType, Action: e.Action, ActorType: string(e.Actor.Type), ActorID: e.Actor.ID,
		ActorDisplay: e.Actor.Display, SourceService: e.SourceService, SourceTopic: e.SourceTopic,
		SourceEventType: e.SourceEventType, SourceEventID: e.SourceEventID, RetentionTier: string(e.RetentionTier),
		IngestMode: string(e.IngestMode), IPAddress: e.IPAddress, UserAgent: e.UserAgent, TraceID: e.TraceID,
		Metadata: e.Metadata,
	}
	if e.Target != nil {
		r.TargetType, r.TargetID = e.Target.Type, e.Target.ID
	}
	if len(r.Metadata) == 0 {
		r.Metadata = json.RawMessage(`{}`)
	}
	return r
}

// Entry converts an archive line back to an entry.
func (r ArchiveRecord) Entry() AuditEntry {
	e := AuditEntry{
		ID: r.ID, OccurredAt: r.OccurredAt, RecordedAt: r.RecordedAt, TenantID: r.TenantID,
		EntryType: r.EntryType, Action: r.Action,
		Actor:         ActorRef{Type: ActorType(r.ActorType), ID: r.ActorID, Display: r.ActorDisplay},
		SourceService: r.SourceService, SourceTopic: r.SourceTopic, SourceEventType: r.SourceEventType,
		SourceEventID: r.SourceEventID, RetentionTier: RetentionTier(r.RetentionTier), IngestMode: IngestMode(r.IngestMode),
		IPAddress: r.IPAddress, UserAgent: r.UserAgent, TraceID: r.TraceID, Metadata: r.Metadata,
	}
	if r.TargetType != "" {
		e.Target = &TargetRef{Type: r.TargetType, ID: r.TargetID}
	}
	return e
}
