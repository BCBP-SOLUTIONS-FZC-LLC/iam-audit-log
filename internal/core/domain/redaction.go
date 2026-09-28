package domain

import (
	"encoding/json"
	"strings"
	"time"
)

// Redaction statuses (audit_redaction_status, LLD §4.1).
const (
	RedactionPending       = "pending"
	RedactionApplied       = "applied"
	RedactionNotApplicable = "not_applicable"
	RedactionMissed        = "missed"
)

// RedactionTriggerUserDeleted is the only redaction trigger consumed
// (decision D-16: TenantOffboarded raises no task; User Profile fans out one
// UserDeleted per scrubbed user, AL-Q14).
const RedactionTriggerUserDeleted = "UserDeleted"

// userDeletedEntryType is the taxonomy row for UserDeleted (§7.1).
const userDeletedEntryType = "user.deleted"

// RedactionRequest is one audit_redaction_tasks row to create in the same
// transaction as its triggering entry (LLD §8.7, AL-INV-12).
type RedactionRequest struct {
	TaskID               string
	TenantID             string
	SubjectID            string // the erased user's sub (matches actor_id / user target_id)
	TriggerEventType     string
	TriggerSourceEventID string // envelope id — uq_redaction_trigger makes redelivery a no-op
}

// RedactionOutcome is apply_redaction()'s result.
type RedactionOutcome struct {
	Status       string
	RowsRedacted int64
}

// RedactionTrigger reports whether e triggers a GDPR redaction, and for
// whom. isTrigger with an empty subject means the event named no usable
// user id, so no task can be created: the caller must surface that loudly.
func RedactionTrigger(e AuditEntry) (subject string, isTrigger bool) {
	if e.EntryType != userDeletedEntryType {
		return "", false
	}
	if e.Target != nil && e.Target.Type == "user" && IsUUID(e.Target.ID) {
		return e.Target.ID, true
	}
	return "", true
}

// RedactionSubjects returns the ids through which e could be about an
// erased subject: its actor and its user target (the D-15 scope). Only a
// security_3y entry is ever redacted (AL-INV-7), so any other tier yields
// none.
func RedactionSubjects(e AuditEntry) []string {
	if e.RetentionTier != TierSecurity3y {
		return nil
	}
	var out []string
	if IsUUID(e.Actor.ID) {
		out = append(out, e.Actor.ID)
	}
	if e.Target != nil && e.Target.Type == "user" && IsUUID(e.Target.ID) && e.Target.ID != e.Actor.ID {
		out = append(out, e.Target.ID)
	}
	return out
}

// RedactedMetadata is the redaction marker. Keys match the SQL
// redaction_marker() in migration 000007: "_redacted", "_redaction_task_id",
// "_redacted_at", plus "_redacted_on_ingest" when a late row is redacted
// before insert (decision D-18, gap 37).
func RedactedMetadata(taskID string, at time.Time, onIngest bool) json.RawMessage {
	m := map[string]any{"_redacted": true, "_redaction_task_id": taskID, "_redacted_at": at.UTC().Format(time.RFC3339Nano)}
	if onIngest {
		m["_redacted_on_ingest"] = true
	}
	b, err := json.Marshal(m)
	if err != nil { // unreachable: a map of string/bool always marshals
		panic("redaction marker: " + err.Error())
	}
	return b
}

// RedactOnIngest returns e redacted for an already-erased subject (D-18):
// metadata is replaced by the marker, and actor_display is cleared when the
// subject is the actor. Ids are kept (§15.5).
func RedactOnIngest(e AuditEntry, subjectID, taskID string, at time.Time) AuditEntry {
	e.Metadata = RedactedMetadata(taskID, at, true)
	if strings.EqualFold(e.Actor.ID, subjectID) {
		e.Actor.Display = ""
	}
	return e
}
