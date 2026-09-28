package domain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// D-16: only user.deleted triggers redaction; the subject is the user
// target's UUID, and an unusable one is still reported as a trigger.
func TestRedactionTrigger(t *testing.T) {
	const uid = "7f3c2d1e-9a8b-4c5d-8e7f-0a1b2c3d4e5f"
	cases := []struct {
		name        string
		e           AuditEntry
		subject     string
		wantTrigger bool
	}{
		{"user target", AuditEntry{EntryType: "user.deleted", Target: &TargetRef{Type: "user", ID: uid}}, uid, true},
		{"non-uuid target", AuditEntry{EntryType: "user.deleted", Target: &TargetRef{Type: "user", ID: "u-1"}}, "", true},
		{"no target", AuditEntry{EntryType: "user.deleted"}, "", true},
		{"other target type", AuditEntry{EntryType: "user.deleted", Target: &TargetRef{Type: "subject", ID: uid}}, "", true},
		{"other entry type", AuditEntry{EntryType: "user.updated", Target: &TargetRef{Type: "user", ID: uid}}, "", false},
		{"tenant offboarded", AuditEntry{EntryType: "tenant.offboarded"}, "", false},
	}
	for _, tc := range cases {
		subject, trigger := RedactionTrigger(tc.e)
		if subject != tc.subject || trigger != tc.wantTrigger {
			t.Errorf("%s: (%q, %v), want (%q, %v)", tc.name, subject, trigger, tc.subject, tc.wantTrigger)
		}
	}
}

// D-15/D-18: the ingest check looks up the actor and the user target, on
// security_3y rows only.
func TestRedactionSubjects(t *testing.T) {
	const a, b = "7f3c2d1e-9a8b-4c5d-8e7f-0a1b2c3d4e5f", "11111111-2222-4333-8444-555555555555"
	sec := func(actorID string, target *TargetRef) AuditEntry {
		return AuditEntry{RetentionTier: TierSecurity3y, Actor: ActorRef{ID: actorID}, Target: target}
	}
	cases := []struct {
		name string
		e    AuditEntry
		want []string
	}{
		{"actor and user target", sec(a, &TargetRef{Type: "user", ID: b}), []string{a, b}},
		{"target is the actor", sec(a, &TargetRef{Type: "user", ID: a}), []string{a}},
		{"non-uuid actor", sec("iam-system", &TargetRef{Type: "user", ID: b}), []string{b}},
		{"non-user target", sec(a, &TargetRef{Type: "tenant", ID: b}), []string{a}},
		{"non-uuid user target", sec("", &TargetRef{Type: "user", ID: "u-1"}), nil},
		{"no actor, no target", sec("", nil), nil},
		{"compliance tier", AuditEntry{RetentionTier: TierCompliance7y, Actor: ActorRef{ID: a}, Target: &TargetRef{Type: "user", ID: b}}, nil},
		{"access tier", AuditEntry{RetentionTier: TierAccess90d, Actor: ActorRef{ID: a}}, nil},
	}
	for _, tc := range cases {
		got := RedactionSubjects(tc.e)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// D-18: the marker keys (parity with SQL redaction_marker() is asserted in
// test/unit).
func TestRedactedMetadata(t *testing.T) {
	at := time.Date(2026, 9, 26, 10, 0, 0, 123, time.FixedZone("x", 3600))
	for _, onIngest := range []bool{false, true} {
		var m map[string]any
		if err := json.Unmarshal(RedactedMetadata("task-1", at, onIngest), &m); err != nil {
			t.Fatal(err)
		}
		if m["_redacted"] != true || m["_redaction_task_id"] != "task-1" {
			t.Errorf("marker = %v", m)
		}
		ts, _ := m["_redacted_at"].(string)
		if parsed, err := time.Parse(time.RFC3339Nano, ts); err != nil || !parsed.Equal(at) || !strings.HasSuffix(ts, "Z") {
			t.Errorf("_redacted_at = %q (err %v), want RFC 3339 UTC", ts, err)
		}
		_, has := m["_redacted_on_ingest"]
		if has != onIngest || (onIngest && m["_redacted_on_ingest"] != true) {
			t.Errorf("onIngest=%v: marker = %v", onIngest, m)
		}
		wantKeys := 3
		if onIngest {
			wantKeys = 4
		}
		if len(m) != wantKeys {
			t.Errorf("onIngest=%v: %d keys, want %d", onIngest, len(m), wantKeys)
		}
	}
}

// D-18: metadata is replaced; actor_display is cleared only on the
// subject's own (actor) rows; ids are kept.
func TestRedactOnIngest(t *testing.T) {
	const subj = "7f3c2d1e-9a8b-4c5d-8e7f-0a1b2c3d4e5f"
	at := time.Now()
	own := AuditEntry{ID: "e1", Actor: ActorRef{Type: ActorUser, ID: subj, Display: "asha@acme.example"},
		Target: &TargetRef{Type: "user", ID: subj}, Metadata: json.RawMessage(`{"email":"asha@acme.example"}`)}
	got := RedactOnIngest(own, strings.ToUpper(subj), "t1", at)
	if got.Actor.Display != "" || got.Actor.ID != subj || got.Target.ID != subj || got.ID != "e1" {
		t.Errorf("own row: %+v", got)
	}
	if strings.Contains(string(got.Metadata), "asha") || !strings.Contains(string(got.Metadata), `"_redacted_on_ingest":true`) {
		t.Errorf("metadata = %s", got.Metadata)
	}
	if own.Actor.Display != "asha@acme.example" {
		t.Error("input entry must not be mutated")
	}

	admin := AuditEntry{Actor: ActorRef{Type: ActorUser, ID: "11111111-2222-4333-8444-555555555555", Display: "admin@acme.example"},
		Target: &TargetRef{Type: "user", ID: subj}, Metadata: json.RawMessage(`{"email":"asha@acme.example"}`)}
	got = RedactOnIngest(admin, subj, "t1", at)
	if got.Actor.Display != "admin@acme.example" || strings.Contains(string(got.Metadata), "asha") {
		t.Errorf("target row: %+v %s", got.Actor, got.Metadata)
	}
}
