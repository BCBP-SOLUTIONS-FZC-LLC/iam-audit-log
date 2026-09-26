package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
	"time"
)

// RetainedTiers are archived to S3 before their partition is dropped;
// access_90d rows expire with the partition and are never archived (LLD
// §8.5, §15.4).
var RetainedTiers = []RetentionTier{TierSecurity3y, TierCompliance7y}

// RetainUntil is the S3 Object Lock retain-until for a row that occurred at
// t: the tier's compiled duration from event time (AL-INV-6, §15.2). An
// object is locked until RetainUntil(tier, its max_occurred_at), so every
// row in it is kept at least its full tier.
func RetainUntil(tier RetentionTier, t time.Time) time.Time {
	t = t.UTC()
	switch tier {
	case TierCompliance7y:
		return t.AddDate(7, 0, 0)
	case TierSecurity3y:
		return t.AddDate(3, 0, 0)
	default:
		return t.AddDate(0, 0, 90)
	}
}

// Archive state statuses (audit_archive_status, LLD §4.1).
const (
	ArchivePending   = "pending"
	ArchiveArchiving = "archiving"
	ArchiveArchived  = "archived"
	ArchiveVerified  = "verified"
	ArchiveDropped   = "dropped"
	ArchiveExpired   = "expired"
	ArchiveFailed    = "failed"
)

// Drop results returned by audit_drop_partition() (migration 000008).
const (
	DropDropped          = "dropped"
	DropNotVerified      = "not_verified"
	DropCountMismatch    = "count_mismatch"
	DropRedactionPending = "redaction_pending"
	DropMissing          = "missing"
)

var partitionNameRE = regexp.MustCompile(`^audit_events_(\d{4})_(\d{2})$`)

// ParsePartitionName returns the UTC month of a monthly partition name
// (audit_events_YYYY_MM); ok=false for anything else (e.g. the DEFAULT).
func ParsePartitionName(name string) (month time.Time, ok bool) {
	if !partitionNameRE.MatchString(name) {
		return time.Time{}, false
	}
	m, err := time.Parse("2006_01", strings.TrimPrefix(name, "audit_events_"))
	if err != nil {
		return time.Time{}, false
	}
	return m.UTC(), true
}

// ArchiveEligible reports whether a month is due for archival (LLD §8.5,
// AL-D4): the whole month is older than the hot window, and it is before
// the trailing writable months.
func ArchiveEligible(month, now time.Time, hotWindowDays, writableTrailingMonths int) bool {
	now = now.UTC()
	monthEnd := month.AddDate(0, 1, 0)
	firstWritable := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -writableTrailingMonths, 0)
	return !monthEnd.After(now.AddDate(0, 0, -hotWindowDays)) && month.Before(firstWritable)
}

// ManifestSHA256 is the (partition, tier) manifest checksum recorded in
// audit_event_archive_state.sha256_manifest: SHA-256 over the sorted
// "key sha256" lines of its objects (§4.2, §15.4).
func ManifestSHA256(objects []ArchiveObject) string {
	lines := make([]string, len(objects))
	for i, o := range objects {
		lines[i] = o.Key + " " + o.SHA256
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

// SubjectIDs returns the distinct subjects of rows, for the manifest's
// subject_ids (D-17): actor ids and user-target ids.
func SubjectIDs(rows []AuditEntry) []string {
	seen := map[string]struct{}{}
	for _, e := range rows {
		if IsUUID(e.Actor.ID) {
			seen[strings.ToLower(e.Actor.ID)] = struct{}{}
		}
		if e.Target != nil && e.Target.Type == "user" && IsUUID(e.Target.ID) {
			seen[strings.ToLower(e.Target.ID)] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
