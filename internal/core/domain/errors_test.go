package domain

import (
	"testing"
	"time"
)

// Every §17 code maps to its frozen HTTP status.
func TestErrorTaxonomy_StatusMapping(t *testing.T) {
	want := map[ErrorCode]int{
		ErrInvalidRequest: 400, ErrUnauthenticated: 401,
		ErrInsufficientPermissions: 403, ErrForbiddenPeer: 403,
		ErrAuditEntryNotFound: 404, ErrExportNotFound: 404,
		ErrUnknownEntryType: 422, ErrInvalidActor: 422, ErrMetadataTooLarge: 422, ErrBatchTooLarge: 422,
		ErrRateLimited: 429, ErrDependencyUnavailable: 503,
	}
	for code, status := range want {
		if got := NewError(code, "m").Status(); got != status {
			t.Errorf("%s: status %d, want %d", code, got, status)
		}
	}
	if got := NewError("not_a_code", "m").Status(); got != 500 {
		t.Errorf("unmapped code: status %d, want 500", got)
	}
	if len(httpStatus) != len(want) {
		t.Errorf("httpStatus has %d codes, test covers %d — keep §17 and this test in sync", len(httpStatus), len(want))
	}
}

func TestError_MessageAndDetails(t *testing.T) {
	e := NewError(ErrInvalidActor, "anonymous with id").WithDetails(map[string]any{"field": "actor.id"})
	if e.Error() != "invalid_actor: anonymous with id" {
		t.Errorf("Error() = %q", e.Error())
	}
	if e.Details["field"] != "actor.id" {
		t.Errorf("details = %v", e.Details)
	}
}

// LLD §4.2: monthly partitions are named audit_events_YYYY_MM by UTC month.
func TestPartitionName_UTCMonth(t *testing.T) {
	loc := time.FixedZone("UTC+5:30", 5*3600+1800)
	// 2026-10-01 03:00 in +05:30 is still 2026-09-30 in UTC.
	if got := PartitionName(time.Date(2026, 10, 1, 3, 0, 0, 0, loc)); got != "audit_events_2026_09" {
		t.Errorf("got %s", got)
	}
}
