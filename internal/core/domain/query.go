package domain

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// Query limits (LLD §5.4 AL-1).
const (
	DefaultQueryLimit = 100
	MaxQueryLimit     = 1000
)

// QueryFilter is the AL-1 / AL-3 filter (LLD §5.4). All fields optional.
type QueryFilter struct {
	From          *time.Time `json:"from,omitempty"`
	To            *time.Time `json:"to,omitempty"`
	EntryTypes    []string   `json:"entry_type,omitempty"`
	ActorID       string     `json:"actor_id,omitempty"`
	ActorType     string     `json:"actor_type,omitempty"`
	TargetType    string     `json:"target_type,omitempty"`
	TargetID      string     `json:"target_id,omitempty"`
	SourceService string     `json:"source_service,omitempty"`
	RetentionTier string     `json:"retention_tier,omitempty"`
}

// Validate checks the filter's own shape (400 invalid_request, §17).
func (f QueryFilter) Validate() error {
	if f.From != nil && f.To != nil && f.From.After(*f.To) {
		return NewError(ErrInvalidRequest, "from must not be after to")
	}
	for _, et := range f.EntryTypes {
		if !ValidEntryTypeShape(et) {
			return NewError(ErrInvalidRequest, "entry_type "+quote(et)+" is not a valid entry type")
		}
	}
	if f.ActorID != "" && !IsUUID(f.ActorID) {
		return NewError(ErrInvalidRequest, "actor_id must be a UUID")
	}
	switch ActorType(f.ActorType) {
	case "", ActorUser, ActorServiceAccount, ActorIAMSystem, ActorAnonymous:
	default:
		return NewError(ErrInvalidRequest, "actor_type must be one of user|service_account|iam_system|anonymous")
	}
	switch RetentionTier(f.RetentionTier) {
	case "", TierCompliance7y, TierSecurity3y, TierAccess90d:
	default:
		return NewError(ErrInvalidRequest, "retention_tier must be one of compliance_7y|security_3y|access_90d")
	}
	if f.TargetID != "" && f.TargetType == "" {
		return NewError(ErrInvalidRequest, "target_id requires target_type")
	}
	return nil
}

// Matches reports whether e satisfies every content filter (used for
// archived rows, which are filtered in memory; RDS rows are filtered in SQL).
func (f QueryFilter) Matches(e AuditEntry) bool {
	if len(f.EntryTypes) > 0 && !contains(f.EntryTypes, e.EntryType) {
		return false
	}
	if f.ActorID != "" && !strings.EqualFold(f.ActorID, e.Actor.ID) {
		return false
	}
	if f.ActorType != "" && f.ActorType != string(e.Actor.Type) {
		return false
	}
	if f.TargetType != "" && (e.Target == nil || e.Target.Type != f.TargetType || (f.TargetID != "" && e.Target.ID != f.TargetID)) {
		return false
	}
	if f.SourceService != "" && f.SourceService != e.SourceService {
		return false
	}
	if f.RetentionTier != "" && f.RetentionTier != string(e.RetentionTier) {
		return false
	}
	return true
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// Cursor is the keyset position (occurred_at DESC, id DESC) — LLD §5.1:
// stable under concurrent ingestion, never offset-based.
type Cursor struct {
	OccurredAt time.Time `json:"t"`
	ID         string    `json:"i"`
}

// Encode returns the opaque, URL-safe cursor string.
func (c Cursor) Encode() string {
	b, err := json.Marshal(c)
	if err != nil { // unreachable: a time and a string always marshal
		panic("cursor: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeCursor parses an opaque cursor (400 invalid_request on garbage).
func DecodeCursor(s string) (*Cursor, error) {
	if s == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	var c Cursor
	if err != nil || json.Unmarshal(raw, &c) != nil || c.OccurredAt.IsZero() || !IsUUID(c.ID) {
		return nil, NewError(ErrInvalidRequest, "cursor is malformed")
	}
	return &c, nil
}

// After reports whether e sorts strictly after the cursor position in
// (occurred_at DESC, id DESC) order — i.e. belongs on a later page.
func (c *Cursor) After(e AuditEntry) bool {
	if c == nil {
		return true
	}
	if !e.OccurredAt.Equal(c.OccurredAt) {
		return e.OccurredAt.Before(c.OccurredAt)
	}
	return e.ID < c.ID
}

// NewerFirst orders two entries by (occurred_at DESC, id DESC).
func NewerFirst(a, b AuditEntry) bool {
	if !a.OccurredAt.Equal(b.OccurredAt) {
		return a.OccurredAt.After(b.OccurredAt)
	}
	return a.ID > b.ID
}

// PlanWindowRow is a tenant's tenant_plan_window projection.
type PlanWindowRow struct {
	PlanCode        string
	QueryWindowDays int
}

// ResolveWindowDays implements LLD §5.4's precedence (AL-D15): the live
// CAT-I2 map value for the row's plan_code; else the row's own stored
// (last-known-good) value; and AUDIT_DEFAULT_QUERY_WINDOW_DAYS only when
// the tenant has no row at all.
func ResolveWindowDays(row *PlanWindowRow, live func(plan string) (int, bool), defaultDays int) int {
	if row == nil {
		return defaultDays
	}
	if live != nil {
		if d, ok := live(row.PlanCode); ok && d > 0 {
			return d
		}
	}
	if row.QueryWindowDays > 0 {
		return row.QueryWindowDays
	}
	return defaultDays
}

// Window is the effective query range after the plan clamp (AL-INV-8).
type Window struct {
	From, To time.Time
	Clamped  bool // the caller's range was cut to the plan window
	Empty    bool // the requested range lies entirely before the window
}

// ClampWindow applies the plan-gated query window (HLD §6.6): the window
// only limits how far back a tenant may query; it never touches storage
// (AL-INV-8). A range entirely older than the window is an empty page, not
// an error.
func ClampWindow(from, to *time.Time, now time.Time, windowDays int) Window {
	earliest := now.Add(-time.Duration(windowDays) * 24 * time.Hour)
	w := Window{From: earliest, To: now}
	if to != nil {
		w.To = *to
	}
	switch {
	case from == nil:
		w.Clamped = true // defaulted to the earliest queryable instant
	case from.Before(earliest):
		w.Clamped = true
	default:
		w.From = *from
	}
	if w.To.Before(earliest) {
		w.Empty, w.Clamped = true, true
	}
	return w
}
