package domain

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

const (
	qTenant = "11111111-1111-1111-1111-111111111111"
	qActor  = "7f3c2d1e-9a8b-4c5d-8e7f-0a1b2c3d4e5f"
	qID1    = "0190a1b2-0000-7000-8000-000000000001"
	qID2    = "0190a1b2-0000-7000-8000-000000000002"
)

func tp(t time.Time) *time.Time { return &t }

// §5.4 AL-1 filter shape → 400 invalid_request.
func TestQueryFilter_Validate(t *testing.T) {
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		f    QueryFilter
		want ErrorCode
	}{
		{"empty", QueryFilter{}, ""},
		{"full", QueryFilter{From: tp(now.Add(-time.Hour)), To: tp(now), EntryTypes: []string{"config.idp.changed"},
			ActorID: qActor, ActorType: "user", TargetType: "tenant", TargetID: "x", SourceService: "svc", RetentionTier: "security_3y"}, ""},
		{"from equals to", QueryFilter{From: tp(now), To: tp(now)}, ""},
		{"from after to", QueryFilter{From: tp(now), To: tp(now.Add(-time.Second))}, ErrInvalidRequest},
		{"bad entry type", QueryFilter{EntryTypes: []string{"Not A Type"}}, ErrInvalidRequest},
		{"bad actor id", QueryFilter{ActorID: "asha"}, ErrInvalidRequest},
		{"bad actor type", QueryFilter{ActorType: "robot"}, ErrInvalidRequest},
		{"every actor type", QueryFilter{ActorType: "anonymous"}, ""},
		{"bad tier", QueryFilter{RetentionTier: "forever"}, ErrInvalidRequest},
		{"target id without type", QueryFilter{TargetID: "x"}, ErrInvalidRequest},
	}
	for _, tc := range cases {
		if got := codeOf(tc.f.Validate()); got != tc.want {
			t.Errorf("%s: code %q, want %q", tc.name, got, tc.want)
		}
	}
}

func sampleEntry() AuditEntry {
	return AuditEntry{
		ID: qID1, OccurredAt: time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC), TenantID: qTenant,
		EntryType: "config.idp.changed", Action: "update",
		Actor:         ActorRef{Type: ActorUser, ID: qActor},
		Target:        &TargetRef{Type: "tenant", ID: "t1"},
		SourceService: "iam-realm-provisioner", RetentionTier: TierCompliance7y,
	}
}

func TestQueryFilter_Matches(t *testing.T) {
	e := sampleEntry()
	noTarget := sampleEntry()
	noTarget.Target = nil
	cases := []struct {
		name string
		f    QueryFilter
		e    AuditEntry
		want bool
	}{
		{"empty filter", QueryFilter{}, e, true},
		{"entry type hit", QueryFilter{EntryTypes: []string{"a.b", "config.idp.changed"}}, e, true},
		{"entry type miss", QueryFilter{EntryTypes: []string{"a.b"}}, e, false},
		{"actor id case-insensitive", QueryFilter{ActorID: "7F3C2D1E-9A8B-4C5D-8E7F-0A1B2C3D4E5F"}, e, true},
		{"actor id miss", QueryFilter{ActorID: qID1}, e, false},
		{"actor type miss", QueryFilter{ActorType: "anonymous"}, e, false},
		{"target type hit", QueryFilter{TargetType: "tenant"}, e, true},
		{"target pair hit", QueryFilter{TargetType: "tenant", TargetID: "t1"}, e, true},
		{"target id miss", QueryFilter{TargetType: "tenant", TargetID: "t2"}, e, false},
		{"target type miss", QueryFilter{TargetType: "user"}, e, false},
		{"target on untargeted row", QueryFilter{TargetType: "tenant"}, noTarget, false},
		{"source miss", QueryFilter{SourceService: "other"}, e, false},
		{"tier hit", QueryFilter{RetentionTier: "compliance_7y"}, e, true},
		{"tier miss", QueryFilter{RetentionTier: "access_90d"}, e, false},
	}
	for _, tc := range cases {
		if got := tc.f.Matches(tc.e); got != tc.want {
			t.Errorf("%s: Matches = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// §5.1: opaque keyset cursor on (occurred_at DESC, id DESC).
func TestCursor_RoundTripAndMalformed(t *testing.T) {
	c := Cursor{OccurredAt: time.Date(2026, 8, 1, 10, 0, 0, 123, time.UTC), ID: qID1}
	got, err := DecodeCursor(c.Encode())
	if err != nil || got == nil || !got.OccurredAt.Equal(c.OccurredAt) || got.ID != c.ID {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
	if got, err := DecodeCursor(""); got != nil || err != nil {
		t.Errorf("empty cursor = %+v, %v; want nil, nil", got, err)
	}
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for _, bad := range []string{
		"!!!not-base64!!!",
		enc("not json"),
		enc(`{"i":"` + qID1 + `"}`),                 // zero time
		enc(`{"t":"2026-08-01T00:00:00Z","i":"x"}`), // id not a UUID
	} {
		if _, err := DecodeCursor(bad); codeOf(err) != ErrInvalidRequest {
			t.Errorf("DecodeCursor(%q) err = %v, want invalid_request", bad, err)
		}
	}
}

func TestCursor_AfterAndNewerFirst(t *testing.T) {
	t0 := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	c := &Cursor{OccurredAt: t0, ID: qID2}
	var nilCursor *Cursor
	if !nilCursor.After(AuditEntry{}) {
		t.Error("nil cursor: every entry is after it")
	}
	cases := []struct {
		e    AuditEntry
		want bool
	}{
		{AuditEntry{OccurredAt: t0.Add(-time.Second), ID: qID2}, true}, // older
		{AuditEntry{OccurredAt: t0.Add(time.Second), ID: qID1}, false}, // newer
		{AuditEntry{OccurredAt: t0, ID: qID1}, true},                   // tie, smaller id
		{AuditEntry{OccurredAt: t0, ID: qID2}, false},                  // the cursor row itself
	}
	for _, tc := range cases {
		if got := c.After(tc.e); got != tc.want {
			t.Errorf("After(%v,%s) = %v, want %v", tc.e.OccurredAt, tc.e.ID, got, tc.want)
		}
	}
	a := AuditEntry{OccurredAt: t0, ID: qID2}
	b := AuditEntry{OccurredAt: t0, ID: qID1}
	older := AuditEntry{OccurredAt: t0.Add(-time.Minute), ID: qID2}
	if !NewerFirst(a, b) || NewerFirst(b, a) || !NewerFirst(b, older) || NewerFirst(older, a) {
		t.Error("NewerFirst must order by occurred_at DESC, id DESC")
	}
}

type liveMap map[string]int

func (m liveMap) get(p string) (int, bool) { d, ok := m[p]; return d, ok }

// §5.4 / AL-D15 precedence: live map → row value → default (no row only).
func TestResolveWindowDays_Precedence_ALD15(t *testing.T) {
	live := liveMap{"pro": 1095, "zero": 0}.get
	cases := []struct {
		name string
		row  *PlanWindowRow
		live func(string) (int, bool)
		want int
	}{
		{"no row → default", nil, live, 365},
		{"live map wins", &PlanWindowRow{PlanCode: "pro", QueryWindowDays: 365}, live, 1095},
		{"map miss → row", &PlanWindowRow{PlanCode: "starter", QueryWindowDays: 400}, live, 400},
		{"map zero ignored → row", &PlanWindowRow{PlanCode: "zero", QueryWindowDays: 400}, live, 400},
		{"nil map → row", &PlanWindowRow{PlanCode: "pro", QueryWindowDays: 400}, nil, 400},
		{"row without days → default", &PlanWindowRow{PlanCode: "starter"}, live, 365},
	}
	for _, tc := range cases {
		if got := ResolveWindowDays(tc.row, tc.live, 365); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
}

// HLD §6.6 clamp arithmetic.
func TestClampWindow(t *testing.T) {
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	earliest := now.AddDate(0, 0, -30)
	inside := now.AddDate(0, 0, -10)
	old := now.AddDate(0, 0, -60)

	w := ClampWindow(nil, nil, now, 30)
	if !w.From.Equal(earliest) || !w.To.Equal(now) || !w.Clamped || w.Empty {
		t.Errorf("defaults: %+v", w)
	}
	w = ClampWindow(&inside, nil, now, 30)
	if !w.From.Equal(inside) || w.Clamped || w.Empty {
		t.Errorf("inside window: %+v", w)
	}
	w = ClampWindow(&old, &inside, now, 30)
	if !w.From.Equal(earliest) || !w.To.Equal(inside) || !w.Clamped || w.Empty {
		t.Errorf("older from: %+v", w)
	}
	oldTo := now.AddDate(0, 0, -45)
	w = ClampWindow(&old, &oldTo, now, 30)
	if !w.Empty || !w.Clamped {
		t.Errorf("range entirely before window must be empty+clamped: %+v", w)
	}
	w = ClampWindow(&earliest, nil, now, 30)
	if w.Clamped || !w.From.Equal(earliest) {
		t.Errorf("from == earliest is not clamped: %+v", w)
	}
}

func TestExportJob_DownloadableAndLapsed_D11(t *testing.T) {
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	future, past := now.Add(time.Hour), now.Add(-time.Hour)
	cases := []struct {
		name                string
		j                   ExportJob
		downloadable, lapse bool
	}{
		{"ready in window", ExportJob{Status: ExportReady, S3Key: "k", SignedURLExpiresAt: &future}, true, false},
		{"ready lapsed", ExportJob{Status: ExportReady, S3Key: "k", SignedURLExpiresAt: &past}, false, true},
		{"ready at boundary", ExportJob{Status: ExportReady, S3Key: "k", SignedURLExpiresAt: &now}, false, true},
		{"ready without key", ExportJob{Status: ExportReady, SignedURLExpiresAt: &future}, false, false},
		{"ready without expiry", ExportJob{Status: ExportReady, S3Key: "k"}, false, false},
		{"pending", ExportJob{Status: ExportPending}, false, false},
		{"expired", ExportJob{Status: ExportExpired, S3Key: "k", SignedURLExpiresAt: &past}, false, false},
	}
	for _, tc := range cases {
		if got := tc.j.Downloadable(now); got != tc.downloadable {
			t.Errorf("%s: Downloadable = %v", tc.name, got)
		}
		if got := tc.j.Lapsed(now); got != tc.lapse {
			t.Errorf("%s: Lapsed = %v", tc.name, got)
		}
	}
}

// D-10 key scheme; LLD §25 export key.
func TestArchiveAndExportKeys_D10(t *testing.T) {
	month := time.Date(2026, 3, 15, 23, 0, 0, 0, time.FixedZone("x", 5*3600))
	got := ArchiveKey(TierSecurity3y, qTenant, month, 7)
	want := "security_3y/" + qTenant + "/2026/03/audit_events_2026_03-part-0007.jsonl.gz"
	if got != want {
		t.Errorf("ArchiveKey = %q, want %q", got, want)
	}
	if got := ExportKey(qTenant, qID1); got != "exports/"+qTenant+"/"+qID1+".jsonl.gz" {
		t.Errorf("ExportKey = %q", got)
	}
}

func TestArchiveRecord_RoundTrip(t *testing.T) {
	e := sampleEntry()
	e.RecordedAt = e.OccurredAt.Add(time.Second)
	e.SourceTopic, e.SourceEventType, e.SourceEventID = "iam.tenant.events", "TenantIdpConfigChanged", "k1"
	e.IngestMode, e.IPAddress, e.UserAgent, e.TraceID = IngestBus, "203.0.113.7", "ua", "tr"
	e.Actor.Display = "asha"
	e.Metadata = json.RawMessage(`{"a":1}`)

	b, err := json.Marshal(ToRecord(e))
	if err != nil {
		t.Fatal(err)
	}
	var r ArchiveRecord
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	back := r.Entry()
	if back.ID != e.ID || !back.OccurredAt.Equal(e.OccurredAt) || !back.RecordedAt.Equal(e.RecordedAt) ||
		back.Actor != e.Actor || back.Target == nil || *back.Target != *e.Target || back.IngestMode != e.IngestMode ||
		back.RetentionTier != e.RetentionTier || back.SourceTopic != e.SourceTopic || back.IPAddress != e.IPAddress ||
		string(back.Metadata) != `{"a":1}` || back.TraceID != "tr" || back.UserAgent != "ua" || back.SourceEventID != "k1" {
		t.Errorf("round trip lost data:\n got %+v\nwant %+v", back, e)
	}

	e.Target, e.Metadata = nil, nil
	r = ToRecord(e)
	if string(r.Metadata) != `{}` || r.TargetType != "" {
		t.Errorf("empty metadata / nil target record = %+v", r)
	}
	if r.Entry().Target != nil {
		t.Error("no target_type → nil Target")
	}
}
