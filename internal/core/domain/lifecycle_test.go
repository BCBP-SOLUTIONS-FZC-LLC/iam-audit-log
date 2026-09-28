package domain

import (
	"strings"
	"testing"
	"time"
)

// §15.2: Object Lock covers each tier's compiled duration from event time.
func TestRetainUntil(t *testing.T) {
	at := time.Date(2026, 3, 14, 9, 12, 4, 0, time.FixedZone("x", 5*3600))
	u := at.UTC()
	for tier, want := range map[RetentionTier]time.Time{
		TierCompliance7y: u.AddDate(7, 0, 0),
		TierSecurity3y:   u.AddDate(3, 0, 0),
		TierAccess90d:    u.AddDate(0, 0, 90),
	} {
		got := RetainUntil(tier, at)
		if !got.Equal(want) || got.Location() != time.UTC {
			t.Errorf("%s: %v, want %v (UTC)", tier, got, want)
		}
	}
}

func TestParsePartitionName(t *testing.T) {
	m, ok := ParsePartitionName("audit_events_2026_03")
	if !ok || !m.Equal(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("valid: %v %v", m, ok)
	}
	for _, bad := range []string{"audit_events_default", "audit_events_2026_13", "audit_events_2026_3", "junk", "", "xaudit_events_2026_03"} {
		if _, ok := ParsePartitionName(bad); ok {
			t.Errorf("%q must not parse", bad)
		}
	}
}

// AL-D4 / §8.5: eligible once the whole month is past the hot window AND
// before the writable trailing months.
func TestArchiveEligible(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) // now-90d = 2026-06-28T12:00; first writable (3) = 2026-06-01
	for _, tc := range []struct {
		month         string
		hot, trailing int
		want          bool
	}{
		{"2026_03", 90, 3, true},
		{"2026_05", 90, 3, true},   // ends 06-01 ≤ 06-28, before 06-01
		{"2026_06", 90, 3, false},  // ends 07-01 > 06-28
		{"2026_05", 90, 4, false},  // first writable 05-01: May is writable
		{"2026_06", 30, 3, false},  // past hot window but writable
		{"2026_06", 30, 2, true},   // ends 07-01 ≤ 08-27, before 07-01
		{"2025_12", 400, 0, false}, // ends 2026-01-01 > now-400d? now-400d = 2025-08-22 → not past
	} {
		m, _ := ParsePartitionName("audit_events_" + tc.month)
		if got := ArchiveEligible(m, now, tc.hot, tc.trailing); got != tc.want {
			t.Errorf("%s hot=%d trailing=%d: %v, want %v", tc.month, tc.hot, tc.trailing, got, tc.want)
		}
	}
	// Exact boundary: the month end equal to now-hot is eligible.
	edge := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, 90)
	if !ArchiveEligible(time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), edge, 90, 0) {
		t.Error("month end == now-hot must be eligible")
	}
}

func TestManifestSHA256(t *testing.T) {
	a := ArchiveObject{Key: "k1", SHA256: "aa"}
	b := ArchiveObject{Key: "k2", SHA256: "bb"}
	x, y := ManifestSHA256([]ArchiveObject{a, b}), ManifestSHA256([]ArchiveObject{b, a})
	if x != y || len(x) != 64 {
		t.Errorf("order-independent 64-hex: %s %s", x, y)
	}
	if x == ManifestSHA256([]ArchiveObject{a, {Key: "k2", SHA256: "bc"}}) {
		t.Error("a changed object checksum must change the manifest")
	}
	if ManifestSHA256(nil) != ManifestSHA256([]ArchiveObject{}) {
		t.Error("empty manifests must agree")
	}
}

// D-17: distinct, lowercased, sorted actor + user-target ids only.
func TestSubjectIDs(t *testing.T) {
	a := "AAAAAAAA-0000-4000-8000-000000000001"
	b := "bbbbbbbb-0000-4000-8000-000000000002"
	c := "cccccccc-0000-4000-8000-000000000003"
	rows := []AuditEntry{
		{Actor: ActorRef{ID: b}, Target: &TargetRef{Type: "user", ID: a}},
		{Actor: ActorRef{ID: strings.ToLower(a)}},
		{Actor: ActorRef{ID: "not-a-uuid"}, Target: &TargetRef{Type: "tender", ID: c}},
		{Actor: ActorRef{}, Target: &TargetRef{Type: "user", ID: "nope"}},
	}
	got := SubjectIDs(rows)
	want := []string{strings.ToLower(a), b}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("subjects = %v, want %v", got, want)
	}
	if got := SubjectIDs(nil); got == nil || len(got) != 0 {
		t.Errorf("empty = %#v, want non-nil empty", got)
	}
}
