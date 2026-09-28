// Package domain holds the Audit Log core model: the audit entry, actor
// model, entry_type taxonomy, retention tiers, and the §17 error taxonomy.
// It imports nothing outside the standard library (LLD §3.2).
package domain

import "fmt"

// ErrorCode is one of the frozen §17 error taxonomy codes, surfaced through
// the flat ErrorResponse shape ({error, status, trace_id, request_id,
// details?}) shared with the sibling IAM services.
type ErrorCode string

// ErrorCode values (LLD §17).
const (
	ErrInvalidRequest          ErrorCode = "invalid_request"          // 400
	ErrUnauthenticated         ErrorCode = "missing_identity_headers" // 401 (gateway identity absent/malformed)
	ErrInsufficientPermissions ErrorCode = "insufficient_permissions" // 403 (non-admin on a query route)
	ErrForbiddenPeer           ErrorCode = "forbidden_peer"           // 403 (unrecognized mesh identity on /internal/*)
	ErrAuditEntryNotFound      ErrorCode = "audit_entry_not_found"    // 404
	ErrExportNotFound          ErrorCode = "export_not_found"         // 404
	ErrUnknownEntryType        ErrorCode = "unknown_entry_type"       // 422
	ErrInvalidActor            ErrorCode = "invalid_actor"            // 422
	ErrMetadataTooLarge        ErrorCode = "metadata_too_large"       // 422
	ErrBatchTooLarge           ErrorCode = "batch_too_large"          // 422
	ErrRangeTooLarge           ErrorCode = "range_too_large"          // 422 (AL-7 archived range over the D-10 bounds; BUILD_PLAN D-14)
	ErrRateLimited             ErrorCode = "rate_limited"             // 429
	ErrDependencyUnavailable   ErrorCode = "dependency_unavailable"   // 503
)

// httpStatus maps every ErrorCode to its frozen HTTP status (§17).
var httpStatus = map[ErrorCode]int{
	ErrInvalidRequest:          400,
	ErrUnauthenticated:         401,
	ErrInsufficientPermissions: 403,
	ErrForbiddenPeer:           403,
	ErrAuditEntryNotFound:      404,
	ErrExportNotFound:          404,
	ErrUnknownEntryType:        422,
	ErrInvalidActor:            422,
	ErrMetadataTooLarge:        422,
	ErrBatchTooLarge:           422,
	ErrRangeTooLarge:           422,
	ErrRateLimited:             429,
	ErrDependencyUnavailable:   503,
}

// Error is this service's domain error type: a stable code, an HTTP status
// derived from that code, a human message, and optional structured details
// echoed verbatim into ErrorResponse.details.
type Error struct {
	Code    ErrorCode
	Message string
	Details map[string]any
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Status returns the frozen HTTP status for e's code (500 if unmapped).
func (e *Error) Status() int {
	if s, ok := httpStatus[e.Code]; ok {
		return s
	}
	return 500
}

// NewError constructs a domain Error for code with message.
func NewError(code ErrorCode, message string) *Error {
	return &Error{Code: code, Message: message}
}

// WithDetails attaches structured details and returns e for chaining.
func (e *Error) WithDetails(details map[string]any) *Error {
	e.Details = details
	return e
}
