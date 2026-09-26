package domain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const (
	tA   = "11111111-1111-1111-1111-111111111111"
	uid  = "7f3c2d1e-9a8b-4c5d-8e7f-0a1b2c3d4e5f"
	opID = "22222222-2222-2222-2222-222222222222"
)

var t0 = time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)

func ev(topic, typ, actor string, payload string) BusEvent {
	return BusEvent{ID: "0190a1b2-0000-7000-8000-000000000001", Type: typ, Source: "producer", Topic: topic,
		TenantID: tA, Actor: actor, Time: t0, Payload: json.RawMessage(payload)}
}

func meta(t *testing.T, e AuditEntry) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(e.Metadata, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// D-8 rule 1 + AL-D11 (iam-event-consumer shapes): the envelope actor is
// always the sentinel; the principal is data.user_id; an unresolvable user
// is anonymous with the claimed username kept only in metadata.
func TestBuildBusEntry_AuthActors_D8_ALD11(t *testing.T) {
	ok := ev(TopicAuth, "LoginSuccess", IAMSystemActorID, `{"user_id":"`+uid+`","realm":"trial","ip_address":"203.0.113.7"}`)
	ok.IPAddress = "203.0.113.7"
	e, known, err := BuildBusEntry(ok, 8192, t0)
	if err != nil || !known {
		t.Fatal(err)
	}
	if e.Actor != (ActorRef{Type: ActorUser, ID: uid}) || e.EntryType != "auth.login.success" || e.RetentionTier != TierAccess90d {
		t.Errorf("entry = %+v", e)
	}
	if e.Target == nil || e.Target.Type != "user" || e.IPAddress != "203.0.113.7" || e.Action != "success" {
		t.Errorf("target/ip/action = %+v %s %s", e.Target, e.IPAddress, e.Action)
	}

	fail := ev(TopicAuth, "LoginFailure", IAMSystemActorID, `{"attempted_username":"mallory@x","error":"user_not_found"}`)
	e, _, _ = BuildBusEntry(fail, 8192, t0)
	if e.Actor != (ActorRef{Type: ActorAnonymous}) || e.Actor.Display != "" {
		t.Errorf("unknown-user LoginFailure must be anonymous with no id/display: %+v", e.Actor)
	}
	if meta(t, e)["attempted_username"] != "mallory@x" || e.RetentionTier != TierCompliance7y {
		t.Errorf("claimed identity must stay in metadata: %s", e.Metadata)
	}

	viaSubject := ev(TopicAuth, "MFAReset", IAMSystemActorID, `{}`)
	viaSubject.Subject = uid
	e, _, _ = BuildBusEntry(viaSubject, 8192, t0)
	if e.Actor.ID != uid {
		t.Errorf("subject fallback: %+v", e.Actor)
	}
}

// D-8 rules 2–4 across the producers' actor spellings.
func TestBuildBusEntry_ActorSpellings_D8(t *testing.T) {
	sys := ActorRef{Type: ActorIAMSystem, ID: IAMSystemActorID}
	cases := []struct {
		name, actor, ip string
		want            ActorRef
		unattributed    bool
	}{
		{"user-profile/O&M/delegation", "iam-system", "", sys, false},
		{"event-consumer/RP/token-service sentinel", IAMSystemActorID, "", sys, false},
		{"O&M nil uuid", NilUUID, "", sys, false},
		{"operator uuid", strings.ToUpper(opID), "", ActorRef{Type: ActorUser, ID: opID}, false},
		{"RP absent actor", "", "", sys, true},
		{"unknown label", "some-bot", "", sys, true},
		{"cron ip sentinel", "iam-system", "system", sys, false},
	}
	for _, tc := range cases {
		b := ev(TopicDelegation, "DelegationEnded", tc.actor, `{"delegation_id":"d-1"}`)
		b.IPAddress = tc.ip
		b.UserAgent = "iam-delegation/delegation-expiry-cron"
		e, _, err := BuildBusEntry(b, 8192, t0)
		if err != nil {
			t.Fatal(err)
		}
		if e.Actor != tc.want {
			t.Errorf("%s: actor = %+v", tc.name, e.Actor)
		}
		m := meta(t, e)
		if _, got := m[MarkerActorUnattributed]; got != tc.unattributed {
			t.Errorf("%s: unattributed marker = %v", tc.name, got)
		}
		if tc.ip == "system" && (e.IPAddress != "" || e.UserAgent != "iam-delegation/delegation-expiry-cron") {
			t.Errorf("§10.3 cron sentinel: ip must be NULL and user_agent kept: %q %q", e.IPAddress, e.UserAgent)
		}
	}
	unknownLabel, _, _ := BuildBusEntry(ev(TopicTenant, "TenantSuspended", "some-bot", `{}`), 8192, t0)
	if meta(t, unknownLabel)[MarkerActorRaw] != "some-bot" {
		t.Error("the raw unrecognized actor label must be preserved")
	}
}

// D-8 targets per topic.
func TestBuildBusEntry_Targets_D8(t *testing.T) {
	cases := []struct {
		topic, typ, payload, wantType, wantID string
	}{
		{TopicMembership, "DepartmentMembershipGranted", `{"user_id":"` + uid + `"}`, "user", uid},
		{TopicMembership, "TenderAssigneeOverridden", `{"tender_id":"t-9","user_id":"` + uid + `"}`, "tender", "t-9"},
		{TopicMembership, "TenantSeatOverageStarted", `{}`, "tenant", tA},
		{TopicTenant, "TenantCreated", `{"tenant_id":"` + tA + `","plan":"pro"}`, "tenant", tA},
		{TopicBilling, "TenantPlanChanged", `{}`, "tenant", tA},
		{TopicUsage, "TenantQuotaWarning", `{}`, "tenant", tA},
		{TopicUser, "UserDeleted", `{"user_id":"` + uid + `"}`, "user", uid},
		{TopicDelegation, "DelegationStarted", `{"delegation_id":"d-1"}`, "delegation", "d-1"},
		{TopicServiceAccount, "ServiceAccountRevoked", `{"principal_id":"p-1"}`, "service_account", "p-1"},
		{TopicTender, "TenderSectionApproved", `{"tender_id":"t-1","signature_hash":"ab"}`, "tender", "t-1"},
		{TopicWFWorkflow, "workflow.task.created", `{"task_id":"k-1","workflow_instance_id":"i-1"}`, "workflow_task", "k-1"},
		{TopicWFWorkflow, "workflow.instance.finished", `{"workflow_instance_id":"i-1"}`, "workflow_instance", "i-1"},
		{TopicWFTemplate, "workflow.template.published", `{"workflow_id":"w-1"}`, "workflow_template", "w-1"},
	}
	for _, tc := range cases {
		e, _, err := BuildBusEntry(ev(tc.topic, tc.typ, "iam-system", tc.payload), 8192, t0)
		if err != nil {
			t.Fatal(err)
		}
		if e.Target == nil || e.Target.Type != tc.wantType || e.Target.ID != tc.wantID {
			t.Errorf("%s/%s: target = %+v, want %s/%s", tc.topic, tc.typ, e.Target, tc.wantType, tc.wantID)
		}
	}
	noTarget, _, _ := BuildBusEntry(ev(TopicAuth, "LoginFailure", IAMSystemActorID, `{}`), 8192, t0)
	if noTarget.Target != nil {
		t.Errorf("no resolvable user → no target: %+v", noTarget.Target)
	}
	withSubject := ev("some.topic", "X", "iam-system", `{}`)
	withSubject.Subject = "instances/1"
	e, _, _ := BuildBusEntry(withSubject, 8192, t0)
	if e.Target == nil || e.Target.Type != "subject" {
		t.Errorf("unmapped topic falls back to subject: %+v", e.Target)
	}
}

// AL-INV-5 provenance + AL-EVT-4 unknown fail-safe + AL-EVT-5 id untouched.
func TestBuildBusEntry_ProvenanceAndUnknown_ALINV5_ALEVT4(t *testing.T) {
	b := ev(TopicMembership, "BrandNewEvent", "iam-system", `{"x":1}`)
	b.Source, b.TraceID = "iam-org-membership-reconciler", "4bf92f3577b34da6a3ce929d0e0e4736"
	e, known, err := BuildBusEntry(b, 8192, t0)
	if err != nil || known {
		t.Fatalf("known=%v err=%v", known, err)
	}
	if e.EntryType != "membership.unknown" || e.RetentionTier != TierSecurity3y || e.Action != "unknown" {
		t.Errorf("unknown classification = %+v", e)
	}
	if e.SourceService != b.Source || e.SourceTopic != TopicMembership || e.SourceEventType != "BrandNewEvent" ||
		e.SourceEventID != b.ID || e.IngestMode != IngestBus || e.TraceID != b.TraceID {
		t.Errorf("provenance = %+v", e)
	}
}

// D-9: an oversized payload is persisted as a truncation marker, never dropped.
func TestBuildBusEntry_OversizedPayloadTruncated_D9(t *testing.T) {
	big := `{"blob":"` + strings.Repeat("x", 9000) + `"}`
	e, _, err := BuildBusEntry(ev(TopicTenant, "TenantSuspended", "", big), 8192, t0)
	if err != nil {
		t.Fatal(err)
	}
	m := meta(t, e)
	if m[MarkerTruncated] != true || m["_original_bytes"].(float64) != float64(len(big)) || len(m["_sha256"].(string)) != 64 {
		t.Errorf("truncation marker = %v", m)
	}
	if m[MarkerActorUnattributed] != true {
		t.Error("markers survive truncation")
	}
	if len(e.Metadata) > 8192 {
		t.Error("metadata must fit the cap")
	}
}

func TestBuildBusEntry_EdgeCases(t *testing.T) {
	if _, _, err := BuildBusEntry(BusEvent{TenantID: tA}, 8192, t0); err == nil {
		t.Error("missing id must fail")
	}
	for _, tenant := range []string{"", "system", "tenants/x"} {
		b := ev(TopicTenant, "TenantCreated", "", `{}`)
		b.TenantID = tenant
		if _, _, err := BuildBusEntry(b, 8192, t0); err == nil {
			t.Errorf("tenant %q must fail (cannot be stored)", tenant)
		}
	}
	zero := ev(TopicUser, "UserUpdated", "iam-system", `[1,2]`)
	zero.Time = time.Time{}
	zero.IPAddress = "not-an-ip"
	now := t0.Add(time.Hour)
	e, _, err := BuildBusEntry(zero, 8192, now)
	if err != nil {
		t.Fatal(err)
	}
	m := meta(t, e)
	if !e.OccurredAt.Equal(now) || m[MarkerOccurredAtMissing] != true {
		t.Errorf("missing time → receipt time + marker: %v %v", e.OccurredAt, m)
	}
	if e.IPAddress != "" || m[MarkerIPRaw] != "not-an-ip" {
		t.Errorf("invalid ip → NULL + raw marker")
	}
	if _, ok := m["_payload"]; !ok {
		t.Error("a non-object payload is wrapped, not lost")
	}
	empty, _, _ := BuildBusEntry(ev(TopicUser, "UserUpdated", "iam-system", ``), 8192, t0)
	if string(empty.Metadata) != "{}" {
		t.Errorf("empty payload → {} got %s", empty.Metadata)
	}
	noSrc := ev(TopicUser, "", "iam-system", `{}`)
	noSrc.Source = ""
	e, _, _ = BuildBusEntry(noSrc, 8192, t0)
	if e.SourceService != "unknown" || e.SourceEventType != "unknown" {
		t.Errorf("NOT NULL provenance defaults: %+v", e)
	}
	if actionFor("nodot") != "nodot" {
		t.Error("actionFor")
	}
}
