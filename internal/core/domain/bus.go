package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"strings"
	"time"
)

// BusEvent is a consumed platform envelope (LLD §7.4), already decoded (Glue
// header stripped, AL-D12) — the transport-neutral input to BuildBusEntry.
type BusEvent struct {
	ID        string // envelope id — the AL-INV-4 dedup key, never rewritten (AL-EVT-5)
	Type      string // producer's verbatim type (AL-INV-5)
	Source    string // producing service
	Topic     string // logical topic the queue subscribes to
	TenantID  string
	Subject   string
	Actor     string
	IPAddress string
	UserAgent string
	TraceID   string
	Time      time.Time
	Payload   json.RawMessage
}

// NilUUID is the all-zero UUID some producers send as a system actor.
const NilUUID = "00000000-0000-0000-0000-000000000000"

// systemActors are the "system" actor spellings producers use (decision
// D-8): user-profile/O&M/delegation send "iam-system", event-consumer/
// realm-provisioner/token-service the …00a1 sentinel, and O&M the nil UUID
// when its caller was iam-system.
var systemActors = map[string]bool{"iam-system": true, IAMSystemActorID: true, NilUUID: true}

// Metadata markers added by BuildBusEntry.
const (
	MarkerActorUnattributed = "_actor_unattributed"
	MarkerActorRaw          = "_actor_raw"
	MarkerIPRaw             = "_ip_address_raw"
	MarkerOccurredAtMissing = "_occurred_at_missing"
	MarkerTruncated         = "_truncated"
)

// BuildBusEntry normalises a consumed event into the single audit row shape
// (AL-INV-2). known=false means the type was unrecognized and classified
// "<domain>.unknown" — persisted, never dropped (AL-EVT-4). The only error
// is an event that cannot be stored at all (no id, or a tenant_id that is
// not a UUID — audit_events.tenant_id is NOT NULL and RLS-scoped); the
// consumer leaves such a message for redelivery → DLQ (RB-1).
func BuildBusEntry(ev BusEvent, maxMetadataBytes int, now time.Time) (entry AuditEntry, known bool, err error) {
	if strings.TrimSpace(ev.ID) == "" {
		return AuditEntry{}, false, NewError(ErrInvalidRequest, "envelope id is missing")
	}
	if !IsUUID(ev.TenantID) {
		return AuditEntry{}, false, NewError(ErrInvalidRequest, "envelope tenant_id "+quote(ev.TenantID)+" is not a UUID")
	}
	entryType, tier, known := ClassifyBus(ev.Topic, ev.Type)

	payload, isObject := decodeObject(ev.Payload)
	markers := map[string]any{}

	actor, ip := deriveActor(ev, payload, markers)
	occurred := ev.Time.UTC()
	if ev.Time.IsZero() {
		occurred = now.UTC()
		markers[MarkerOccurredAtMissing] = true
	}

	meta := buildMetadata(ev.Payload, payload, isObject, markers, maxMetadataBytes)

	return AuditEntry{
		OccurredAt:      occurred,
		TenantID:        strings.ToLower(ev.TenantID),
		EntryType:       entryType,
		Action:          actionFor(entryType),
		Actor:           actor,
		Target:          deriveTarget(ev, entryType, payload),
		SourceService:   nonEmpty(ev.Source, "unknown"),
		SourceTopic:     ev.Topic,
		SourceEventType: nonEmpty(ev.Type, "unknown"),
		SourceEventID:   ev.ID,
		RetentionTier:   tier,
		IngestMode:      IngestBus,
		IPAddress:       ip,
		UserAgent:       ev.UserAgent,
		TraceID:         ev.TraceID,
		Metadata:        meta,
	}, known, nil
}

// deriveActor implements decision D-8 and returns the actor plus the ip to
// store ("" → NULL).
func deriveActor(ev BusEvent, payload, markers map[string]any) (actor ActorRef, ip string) {
	ip = ev.IPAddress
	cronOrigin := ip == "system" // §10.3 event-level cron sentinel
	if cronOrigin {
		ip = ""
	} else if ip != "" && net.ParseIP(ip) == nil {
		markers[MarkerIPRaw] = ip
		ip = ""
	}

	// Rule 1: auth events carry the sentinel as envelope actor; the principal
	// is the Keycloak user. No resolvable user → anonymous (AL-D11): the
	// claimed identity (attempted_username) stays in metadata only.
	if ev.Topic == TopicAuth && !cronOrigin {
		uid := str(payload, "user_id")
		if uid == "" {
			uid = ev.Subject
		}
		if IsUUID(uid) {
			return ActorRef{Type: ActorUser, ID: strings.ToLower(uid)}, ip
		}
		return ActorRef{Type: ActorAnonymous}, ip
	}

	a := strings.TrimSpace(ev.Actor)
	switch {
	case cronOrigin || systemActors[strings.ToLower(a)]: // rule 2
		return ActorRef{Type: ActorIAMSystem, ID: IAMSystemActorID}, ip
	case IsUUID(a): // rule 3
		return ActorRef{Type: ActorUser, ID: strings.ToLower(a)}, ip
	default: // rule 4 — keep the gap visible
		markers[MarkerActorUnattributed] = true
		if a != "" {
			markers[MarkerActorRaw] = a
		}
		return ActorRef{Type: ActorIAMSystem, ID: IAMSystemActorID}, ip
	}
}

// deriveTarget picks what was acted on, per topic (decision D-8).
func deriveTarget(ev BusEvent, entryType string, p map[string]any) *TargetRef {
	pick := func(typ string, keys ...string) *TargetRef {
		for _, k := range keys {
			if v := str(p, k); v != "" {
				return &TargetRef{Type: typ, ID: v}
			}
		}
		return nil
	}
	tenant := &TargetRef{Type: "tenant", ID: strings.ToLower(ev.TenantID)}
	switch ev.Topic {
	case TopicAuth, TopicUser:
		if t := pick("user", "user_id"); t != nil {
			return t
		}
		if IsUUID(ev.Subject) {
			return &TargetRef{Type: "user", ID: ev.Subject}
		}
		return nil
	case TopicMembership:
		switch {
		case entryType == "tender.assignee.overridden":
			return pick("tender", "tender_id")
		case strings.HasPrefix(entryType, "tenant."):
			if t := pick("tenant", "tenant_id"); t != nil {
				return t
			}
			return tenant
		}
		return pick("user", "user_id")
	case TopicTenant, TopicBilling, TopicUsage:
		if t := pick("tenant", "tenant_id"); t != nil {
			return t
		}
		return tenant
	case TopicDelegation:
		if t := pick("delegation", "delegation_id"); t != nil {
			return t
		}
	case TopicServiceAccount:
		return pick("service_account", "principal_id")
	case TopicTender:
		return pick("tender", "tender_id")
	case TopicWFWorkflow:
		if t := pick("workflow_task", "task_id"); t != nil {
			return t
		}
		return pick("workflow_instance", "workflow_instance_id")
	case TopicWFTemplate:
		return pick("workflow_template", "workflow_id")
	}
	if ev.Subject != "" {
		return &TargetRef{Type: "subject", ID: ev.Subject}
	}
	return nil
}

// buildMetadata stores the payload (plus markers) as the metadata object,
// replacing it with a truncation marker when it exceeds the cap (D-9).
func buildMetadata(raw json.RawMessage, payload map[string]any, isObject bool, markers map[string]any, maxBytes int) json.RawMessage {
	obj := map[string]any{}
	switch {
	case isObject:
		for k, v := range payload {
			obj[k] = v
		}
	case strings.TrimSpace(string(raw)) != "" && string(raw) != "null":
		var v any
		if json.Unmarshal(raw, &v) == nil {
			obj["_payload"] = v // non-object payloads are wrapped, not lost
		}
	}
	for k, v := range markers {
		obj[k] = v
	}
	out, err := json.Marshal(obj)
	if err == nil && (maxBytes <= 0 || len(out) <= maxBytes) {
		return out
	}
	sum := sha256.Sum256(raw)
	trunc := map[string]any{
		MarkerTruncated:   true,
		"_original_bytes": len(raw),
		"_sha256":         hex.EncodeToString(sum[:]),
	}
	for k, v := range markers {
		trunc[k] = v
	}
	if out, err = json.Marshal(trunc); err != nil {
		return json.RawMessage(`{"_truncated":true}`)
	}
	return out
}

// actionFor derives the coarse verb from the entry_type's final segment
// (e.g. membership.role.granted → "granted"); the LLD fixes no per-type verb.
func actionFor(entryType string) string {
	if i := strings.LastIndexByte(entryType, '.'); i >= 0 && i < len(entryType)-1 {
		return entryType[i+1:]
	}
	return entryType
}

func decodeObject(raw json.RawMessage) (map[string]any, bool) {
	var m map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil || m == nil {
		return nil, false
	}
	return m, true
}

func str(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func nonEmpty(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func quote(s string) string { return `"` + s + `"` }
