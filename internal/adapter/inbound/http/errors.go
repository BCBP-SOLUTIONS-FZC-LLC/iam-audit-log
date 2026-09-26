package http

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/gin-gonic/gin"
)

// errorLogger is the shared gincommon-backed Logger, set once by NewRouter.
var errorLogger port.Logger

// ErrorResponse is the error body (LLD §17). It is a superset of
// gincommon.ErrorResponse ({error, status, trace_id, request_id}) that also
// carries the LLD's {code, message} fields — `error` and `code` hold the
// same §17 code, matching iam-catalog-admin's envelope.
type ErrorResponse struct {
	Error     string         `json:"error"`
	Code      string         `json:"code"`
	Message   string         `json:"message,omitempty"`
	Status    int            `json:"status"`
	TraceID   string         `json:"trace_id,omitempty"`
	RequestID string         `json:"request_id,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
}

func newErrorResponse(c *gin.Context, code domain.ErrorCode, message string, status int) ErrorResponse {
	er := ErrorResponse{Error: string(code), Code: string(code), Message: message, Status: status}
	if c != nil {
		er.TraceID = gincommon.TraceIDFromContext(c)
		if rid := gincommon.RequestIDFromContext(c); rid != "" {
			er.RequestID = rid
		} else if rid := c.GetHeader(gincommon.HeaderRequestID); rid != "" {
			er.RequestID = rid
		} else if rid := c.Writer.Header().Get(gincommon.HeaderRequestIDResponse); rid != "" {
			er.RequestID = rid
		}
	}
	return er
}

// abort writes a §17 error and stops the handler chain.
func abort(c *gin.Context, code domain.ErrorCode, message string) {
	de := domain.NewError(code, message)
	er := newErrorResponse(c, code, message, de.Status())
	c.AbortWithStatusJSON(er.Status, er)
}

// HandleError maps a *domain.Error to its frozen §17 status/code; a leaked
// connectivity-class PgError maps to 503 dependency_unavailable; anything
// else is a logged 500 internal_error (never silent).
func HandleError(c *gin.Context, err error) {
	var de *domain.Error
	if errors.As(err, &de) {
		er := newErrorResponse(c, de.Code, de.Message, de.Status())
		er.Details = de.Details
		c.AbortWithStatusJSON(er.Status, er)
		return
	}
	if pgcommon.IsConnectionException(err) || pgcommon.IsInsufficientResources(err) {
		abort(c, domain.ErrDependencyUnavailable, "database unavailable")
		return
	}
	if errorLogger != nil {
		errorLogger.Error("unhandled 500 error", map[string]any{"error_type": fmt.Sprintf("%T", err), "error": err.Error()})
	}
	er := newErrorResponse(c, "internal_error", "internal error", http.StatusInternalServerError)
	c.AbortWithStatusJSON(http.StatusInternalServerError, er)
}
