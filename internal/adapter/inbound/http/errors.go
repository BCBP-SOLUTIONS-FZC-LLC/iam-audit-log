package http

import (
	"errors"

	"github.com/gin-gonic/gin"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/problem"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// errorLogger is the shared gincommon-backed Logger, set once by NewRouter.
// Used by ingest_handler.go for per-entry batch error logging.
var errorLogger port.Logger

// ProblemResponse is the RFC 9457 error body (application/problem+json) served
// on every error route. Used in Swag @Failure annotations; the wire format is
// owned entirely by platform-gincommon.
type ProblemResponse = problem.Problem

// Domain problem types for the LLD §17 error taxonomy. Each type carries a
// stable type URI ({DefaultTypeBase}/iam-audit-log/{name}), an HTTP status and
// an upper-snake-case errorCode clients branch on.
var (
	typeInvalidRequest          = problem.MustType("iam-audit-log", "invalid-request", "Invalid Request", 400, "INVALID_REQUEST")
	typeMissingIdentityHeaders  = problem.MustType("iam-audit-log", "missing-identity-headers", "Unauthorized", 401, "MISSING_IDENTITY_HEADERS")
	typeInsufficientPermissions = problem.MustType("iam-audit-log", "insufficient-permissions", "Forbidden", 403, "INSUFFICIENT_PERMISSIONS")
	typeForbiddenPeer           = problem.MustType("iam-audit-log", "forbidden-peer", "Forbidden", 403, "FORBIDDEN_PEER")
	typeAuditEntryNotFound      = problem.MustType("iam-audit-log", "audit-entry-not-found", "Not Found", 404, "AUDIT_ENTRY_NOT_FOUND")
	typeExportNotFound          = problem.MustType("iam-audit-log", "export-not-found", "Not Found", 404, "EXPORT_NOT_FOUND")
	typeUnknownEntryType        = problem.MustType("iam-audit-log", "unknown-entry-type", "Unprocessable Entity", 422, "UNKNOWN_ENTRY_TYPE")
	typeInvalidActor            = problem.MustType("iam-audit-log", "invalid-actor", "Unprocessable Entity", 422, "INVALID_ACTOR")
	typeMetadataTooLarge        = problem.MustType("iam-audit-log", "metadata-too-large", "Unprocessable Entity", 422, "METADATA_TOO_LARGE")
	typeBatchTooLarge           = problem.MustType("iam-audit-log", "batch-too-large", "Unprocessable Entity", 422, "BATCH_TOO_LARGE")
	typeRangeTooLarge           = problem.MustType("iam-audit-log", "range-too-large", "Unprocessable Entity", 422, "RANGE_TOO_LARGE")
	typeRateLimited             = problem.MustType("iam-audit-log", "rate-limited", "Too Many Requests", 429, "RATE_LIMITED")
	typeDependencyUnavailable   = problem.MustType("iam-audit-log", "dependency-unavailable", "Service Unavailable", 503, "DEPENDENCY_UNAVAILABLE")
)

// codeToType maps every LLD §17 ErrorCode to its registered problem.Type.
var codeToType = map[domain.ErrorCode]problem.Type{
	domain.ErrInvalidRequest:          typeInvalidRequest,
	domain.ErrUnauthenticated:         typeMissingIdentityHeaders,
	domain.ErrInsufficientPermissions: typeInsufficientPermissions,
	domain.ErrForbiddenPeer:           typeForbiddenPeer,
	domain.ErrAuditEntryNotFound:      typeAuditEntryNotFound,
	domain.ErrExportNotFound:          typeExportNotFound,
	domain.ErrUnknownEntryType:        typeUnknownEntryType,
	domain.ErrInvalidActor:            typeInvalidActor,
	domain.ErrMetadataTooLarge:        typeMetadataTooLarge,
	domain.ErrBatchTooLarge:           typeBatchTooLarge,
	domain.ErrRangeTooLarge:           typeRangeTooLarge,
	domain.ErrRateLimited:             typeRateLimited,
	domain.ErrDependencyUnavailable:   typeDependencyUnavailable,
}

// toProblem converts a *domain.Error to a *problem.Error using the §17 type
// registry. Unmapped codes fall back to a 500 internal-error (cause never sent).
func toProblem(de *domain.Error) *problem.Error {
	t, ok := codeToType[de.Code]
	if !ok {
		return problem.NewInternalError(de)
	}
	return problem.NewError(t, de.Message)
}

// abort is a package-internal helper for middleware that must immediately stop
// the handler chain with a problem response.
func abort(c *gin.Context, code domain.ErrorCode, message string) {
	gincommon.AbortWithProblem(c, toProblem(domain.NewError(code, message)))
}

// HandleError maps a *domain.Error to its RFC 9457 problem; a leaked
// connectivity-class PgError maps to 503 dependency_unavailable; anything else
// is a 500 internal-error (detail-free, logged by the platform).
func HandleError(c *gin.Context, err error) {
	var de *domain.Error
	if errors.As(err, &de) {
		gincommon.AbortWithProblem(c, toProblem(de))
		return
	}
	if pgcommon.IsConnectionException(err) || pgcommon.IsInsufficientResources(err) {
		gincommon.AbortWithProblem(c, toProblem(domain.NewError(domain.ErrDependencyUnavailable, "database unavailable")))
		return
	}
	gincommon.AbortWithProblem(c, problem.NewInternalError(err))
}
