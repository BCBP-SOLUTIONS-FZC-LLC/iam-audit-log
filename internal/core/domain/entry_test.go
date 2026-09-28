package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func codeOf(err error) ErrorCode {
	var de *Error
	if errors.As(err, &de) {
		return de.Code
	}
	return ""
}

// §10.3 / AL-D11 actor matrix.
func TestActorRef_Validate_ALD11(t *testing.T) {
	uid := "7f3c2d1e-9a8b-4c5d-8e7f-0a1b2c3d4e5f"
	cases := []struct {
		actor ActorRef
		want  ErrorCode
	}{
		{ActorRef{Type: ActorUser, ID: uid}, ""},
		{ActorRef{Type: ActorServiceAccount, ID: uid}, ""},
		{ActorRef{Type: ActorIAMSystem, ID: IAMSystemActorID}, ""},
		{ActorRef{Type: ActorAnonymous}, ""},
		{ActorRef{Type: ActorAnonymous, ID: uid}, ErrInvalidActor},      // claimed identity never promoted
		{ActorRef{Type: ActorUser}, ErrInvalidActor},                    // non-anonymous needs an id
		{ActorRef{Type: ActorIAMSystem, ID: uid}, ErrInvalidActor},      // iam_system ⇒ the sentinel
		{ActorRef{Type: ActorUser, ID: "asha@acme"}, ErrInvalidRequest}, // not a UUID
		{ActorRef{Type: "robot", ID: uid}, ErrInvalidRequest},           // unknown type
	}
	for _, tc := range cases {
		if got := codeOf(tc.actor.Validate()); got != tc.want {
			t.Errorf("%+v: code %q, want %q", tc.actor, got, tc.want)
		}
	}
}

func validCmd() DirectWriteCommand {
	return DirectWriteCommand{
		IdempotencyKey: "key-1", TenantID: "11111111-1111-1111-1111-111111111111",
		EntryType: "config.tenant_setting.changed", Action: "update",
		Actor:         ActorRef{Type: ActorUser, ID: "7F3C2D1E-9A8B-4C5D-8E7F-0A1B2C3D4E5F"},
		Target:        &TargetRef{Type: "tenant_setting", ID: "mfa_freshness_seconds"},
		OccurredAt:    time.Date(2026, 3, 14, 9, 12, 4, 0, time.FixedZone("x", 3600)),
		SourceService: "iam-org-membership", SourceEventType: "TenantSettingChanged",
		IPAddress: "203.0.113.7", Metadata: json.RawMessage(`{ "setting": "mfa_freshness_seconds", "from": 900, "to": 1800 }`),
	}
}

// AL-INV-6 / AL-INV-11: the tier is derived from entry_type; the row is a
// direct_write with no topic, UTC time, lower-cased ids, compact metadata.
func TestBuildDirectWriteEntry_DerivesTierAndShape_ALINV6_ALINV11(t *testing.T) {
	e, err := BuildDirectWriteEntry(validCmd(), 8192)
	if err != nil {
		t.Fatal(err)
	}
	if e.RetentionTier != TierSecurity3y || e.IngestMode != IngestDirectWrite || e.SourceTopic != "" {
		t.Errorf("entry = %+v", e)
	}
	if e.SourceEventID != "key-1" || e.OccurredAt.Location() != time.UTC {
		t.Errorf("source_event_id/occurred_at = %s / %v", e.SourceEventID, e.OccurredAt)
	}
	if e.Actor.ID != "7f3c2d1e-9a8b-4c5d-8e7f-0a1b2c3d4e5f" {
		t.Errorf("actor id not normalised: %s", e.Actor.ID)
	}
	if string(e.Metadata) != `{"setting":"mfa_freshness_seconds","from":900,"to":1800}` {
		t.Errorf("metadata = %s", e.Metadata)
	}

	c := validCmd()
	c.EntryType = "security.cross_tenant_access"
	e, _ = BuildDirectWriteEntry(c, 8192)
	if e.RetentionTier != TierCompliance7y {
		t.Errorf("cross_tenant_access tier = %s, want compliance_7y", e.RetentionTier)
	}
}

func TestBuildDirectWriteEntry_Rejections(t *testing.T) {
	mut := func(f func(*DirectWriteCommand)) DirectWriteCommand { c := validCmd(); f(&c); return c }
	cases := map[string]struct {
		cmd  DirectWriteCommand
		want ErrorCode
	}{
		"no key":            {mut(func(c *DirectWriteCommand) { c.IdempotencyKey = "" }), ErrInvalidRequest},
		"long key":          {mut(func(c *DirectWriteCommand) { c.IdempotencyKey = strings.Repeat("k", 257) }), ErrInvalidRequest},
		"bad tenant":        {mut(func(c *DirectWriteCommand) { c.TenantID = "acme" }), ErrInvalidRequest},
		"no action":         {mut(func(c *DirectWriteCommand) { c.Action = " " }), ErrInvalidRequest},
		"no source_service": {mut(func(c *DirectWriteCommand) { c.SourceService = "" }), ErrInvalidRequest},
		"no occurred_at":    {mut(func(c *DirectWriteCommand) { c.OccurredAt = time.Time{} }), ErrInvalidRequest},
		"unknown type":      {mut(func(c *DirectWriteCommand) { c.EntryType = "config.nope" }), ErrUnknownEntryType},
		"bus type (D-6)":    {mut(func(c *DirectWriteCommand) { c.EntryType = "tender.section.approved" }), ErrUnknownEntryType},
		"unknown fallback":  {mut(func(c *DirectWriteCommand) { c.EntryType = "auth.unknown" }), ErrUnknownEntryType},
		"invalid actor":     {mut(func(c *DirectWriteCommand) { c.Actor = ActorRef{Type: ActorAnonymous, ID: c.TenantID} }), ErrInvalidActor},
		"target w/o type":   {mut(func(c *DirectWriteCommand) { c.Target = &TargetRef{ID: "x"} }), ErrInvalidRequest},
		"bad ip":            {mut(func(c *DirectWriteCommand) { c.IPAddress = "999.1.1.1" }), ErrInvalidRequest},
		"array metadata":    {mut(func(c *DirectWriteCommand) { c.Metadata = json.RawMessage(`[1]`) }), ErrInvalidRequest},
		"broken metadata":   {mut(func(c *DirectWriteCommand) { c.Metadata = json.RawMessage(`{"a":`) }), ErrInvalidRequest},
		"huge metadata":     {mut(func(c *DirectWriteCommand) { c.Metadata = json.RawMessage(`{"a":"` + strings.Repeat("x", 9000) + `"}`) }), ErrMetadataTooLarge},
	}
	for name, tc := range cases {
		if _, err := BuildDirectWriteEntry(tc.cmd, 8192); codeOf(err) != tc.want {
			t.Errorf("%s: code %q, want %q (%v)", name, codeOf(err), tc.want, err)
		}
	}
}

// §4.2 metadata cap and shape: absent/null → {}, measured compact.
func TestNormalizeMetadata(t *testing.T) {
	for _, in := range []string{"", "null", "  "} {
		out, err := NormalizeMetadata(json.RawMessage(in), 10)
		if err != nil || string(out) != "{}" {
			t.Errorf("%q → %s, %v", in, out, err)
		}
	}
	// 12 bytes of whitespace-padded JSON compacts to 9 — fits a 9-byte cap.
	out, err := NormalizeMetadata(json.RawMessage(`{ "a" : 1 , "b":2 }`), 13)
	if err != nil || string(out) != `{"a":1,"b":2}` {
		t.Errorf("compact: %s, %v", out, err)
	}
	_, err = NormalizeMetadata(json.RawMessage(`{"a":1,"b":2}`), 12)
	if codeOf(err) != ErrMetadataTooLarge {
		t.Errorf("cap: %v", err)
	}
	if IsUUID("not") || !IsUUID(IAMSystemActorID) {
		t.Error("IsUUID")
	}
}
