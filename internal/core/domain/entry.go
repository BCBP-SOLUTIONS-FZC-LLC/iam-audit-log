package domain

import (
	"bytes"
	"encoding/json"
	"net"
	"strings"
	"time"
)

// IngestMode is how an entry reached the sink; it never changes the record
// shape (AL-INV-2).
type IngestMode string

// IngestMode values — the audit_ingest_mode enum.
const (
	IngestBus         IngestMode = "bus"
	IngestDirectWrite IngestMode = "direct_write"
)

// ConsumerDirectWrite is the processed_events.consumer discriminator for
// AL-5/AL-6 (LLD §7.5, §25).
const ConsumerDirectWrite = "direct_write"

// TargetRef is what was acted on (nil for tenant-wide actions).
type TargetRef struct {
	Type string
	ID   string
}

// AuditEntry is one audit_events row (LLD §4.2) — the single shape every
// transport produces (AL-INV-2).
type AuditEntry struct {
	ID              string // UUIDv7
	OccurredAt      time.Time
	RecordedAt      time.Time // set by the store
	TenantID        string
	EntryType       string
	Action          string
	Actor           ActorRef
	Target          *TargetRef
	SourceService   string
	SourceTopic     string // "" for direct_write (stored NULL)
	SourceEventType string // producer's verbatim type (AL-INV-5)
	SourceEventID   string // envelope id or idempotency key (AL-INV-4/5)
	RetentionTier   RetentionTier
	IngestMode      IngestMode
	IPAddress       string // "" → NULL
	UserAgent       string
	TraceID         string
	Metadata        json.RawMessage // always a compact JSON object
}

// DirectWriteCommand is one AL-5/AL-6 entry as the caller sent it (LLD
// §5.4). There is deliberately no tier field: the tier is derived from
// entry_type server-side and any caller-supplied value is ignored
// (AL-INV-6, AL-INV-11).
type DirectWriteCommand struct {
	IdempotencyKey  string
	TenantID        string
	EntryType       string
	Action          string
	Actor           ActorRef
	Target          *TargetRef
	OccurredAt      time.Time
	SourceService   string
	SourceEventType string
	IPAddress       string
	UserAgent       string
	TraceID         string
	Metadata        json.RawMessage
}

// MaxIdempotencyKeyLen bounds the key (it becomes source_event_id).
const MaxIdempotencyKeyLen = 256

// BuildDirectWriteEntry validates cmd and returns the audit row it
// produces, minus the store-assigned id/recorded_at. Errors are §17 domain
// errors: invalid_request (400), unknown_entry_type / invalid_actor /
// metadata_too_large (422).
func BuildDirectWriteEntry(cmd DirectWriteCommand, maxMetadataBytes int) (AuditEntry, error) {
	var zero AuditEntry
	if cmd.IdempotencyKey == "" || len(cmd.IdempotencyKey) > MaxIdempotencyKeyLen {
		return zero, NewError(ErrInvalidRequest, "idempotency key is required (1..256 chars)")
	}
	if !IsUUID(cmd.TenantID) {
		return zero, NewError(ErrInvalidRequest, "tenant_id is required and must be a UUID")
	}
	for field, v := range map[string]string{
		"entry_type": cmd.EntryType, "action": cmd.Action,
		"source_service": cmd.SourceService, "source_event_type": cmd.SourceEventType,
	} {
		if strings.TrimSpace(v) == "" {
			return zero, NewError(ErrInvalidRequest, field+" is required")
		}
	}
	if cmd.OccurredAt.IsZero() {
		return zero, NewError(ErrInvalidRequest, "occurred_at is required (RFC 3339)")
	}
	if !IsDirectWriteEntryType(cmd.EntryType) {
		return zero, NewError(ErrUnknownEntryType, "entry_type "+cmd.EntryType+" is not a direct-write taxonomy value (LLD §7.1)")
	}
	tier, _ := TierFor(cmd.EntryType)
	if err := cmd.Actor.Validate(); err != nil {
		return zero, err
	}
	if cmd.Target != nil && strings.TrimSpace(cmd.Target.Type) == "" {
		return zero, NewError(ErrInvalidRequest, "target.type is required when target is present")
	}
	if cmd.IPAddress != "" && net.ParseIP(cmd.IPAddress) == nil {
		return zero, NewError(ErrInvalidRequest, "ip_address must be an IPv4 or IPv6 address")
	}
	meta, err := NormalizeMetadata(cmd.Metadata, maxMetadataBytes)
	if err != nil {
		return zero, err
	}
	return AuditEntry{
		OccurredAt:      cmd.OccurredAt.UTC(),
		TenantID:        strings.ToLower(cmd.TenantID),
		EntryType:       cmd.EntryType,
		Action:          cmd.Action,
		Actor:           ActorRef{Type: cmd.Actor.Type, ID: strings.ToLower(cmd.Actor.ID), Display: cmd.Actor.Display},
		Target:          cmd.Target,
		SourceService:   cmd.SourceService,
		SourceEventType: cmd.SourceEventType,
		SourceEventID:   cmd.IdempotencyKey,
		RetentionTier:   tier,
		IngestMode:      IngestDirectWrite,
		IPAddress:       cmd.IPAddress,
		UserAgent:       cmd.UserAgent,
		TraceID:         cmd.TraceID,
		Metadata:        meta,
	}, nil
}

// NormalizeMetadata returns raw as a compact JSON object ({} when absent),
// enforcing the object shape (chk_metadata_object) and the size cap
// (MAX_METADATA_BYTES, measured on the compact form).
func NormalizeMetadata(raw json.RawMessage, maxBytes int) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return json.RawMessage(`{}`), nil
	}
	if trimmed[0] != '{' {
		return nil, NewError(ErrInvalidRequest, "metadata must be a JSON object")
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, trimmed); err != nil {
		return nil, NewError(ErrInvalidRequest, "metadata is not valid JSON")
	}
	if maxBytes > 0 && buf.Len() > maxBytes {
		return nil, NewError(ErrMetadataTooLarge, "metadata exceeds the size cap").
			WithDetails(map[string]any{"limit_bytes": maxBytes, "size_bytes": buf.Len()})
	}
	return json.RawMessage(buf.Bytes()), nil
}
