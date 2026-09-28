package http

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/pkg/requestctx"
)

// Querier is the AL-1/AL-2/AL-7 use case (implemented by *service.QueryService).
type Querier interface {
	Query(ctx context.Context, req service.QueryRequest) (service.QueryResult, error)
	InternalQuery(ctx context.Context, req service.QueryRequest) (service.QueryResult, error)
	Get(ctx context.Context, tenantID, id string) (domain.AuditEntry, error)
}

// Exporter is the AL-3/AL-4 use case (implemented by *service.ExportService).
type Exporter interface {
	Request(ctx context.Context, tenantID, requestedBy string, f domain.QueryFilter) (domain.ExportJob, error)
	Status(ctx context.Context, tenantID, id string) (service.ExportView, error)
}

// QueryHandler serves the tenant-admin query/export API (LLD §5.4 AL-1..AL-4).
// Tenant and caller always come from the gateway identity, never from the
// path or body (§5.1).
type QueryHandler struct {
	query   Querier
	exports Exporter
	// limiter is the AL-3 per-tenant bucket; a D-10 deferral from AL-1
	// creates an export too, so it draws from the same bucket.
	limiter *TenantRateLimiter
}

// NewQueryHandler wires the handler; limiter may be nil (unlimited).
func NewQueryHandler(query Querier, exports Exporter, limiter *TenantRateLimiter) *QueryHandler {
	return &QueryHandler{query: query, exports: exports, limiter: limiter}
}

func identity(c *gin.Context) (tenantID, userID string) {
	rc, _ := requestctx.FromContext(c.Request.Context())
	return rc.TenantID.String(), rc.UserID.String()
}

func (h *QueryHandler) allowExport(c *gin.Context, tenantID string) bool {
	if h.limiter != nil && !h.limiter.Allow(tenantID) {
		c.Header("Retry-After", "60")
		abort(c, domain.ErrRateLimited, "export rate limit exceeded for this tenant")
		return false
	}
	return true
}

// ListEvents is AL-1.
//
//	@Summary		Search the tenant's audit trail (AL-1)
//	@Description	Keyset-paginated (occurred_at DESC, id DESC) search within the plan window (AL-INV-8). A range older than the window is clamped (window_clamped=true, effective_from). Archived months are read from S3 when the estimate is within ARCHIVE_SYNC_MAX_ROWS / ARCHIVE_SYNC_MAX_BYTES; otherwise an export is created and 202 returned (decision D-10).
//	@Tags			Audit
//	@Produce		json
//	@Security		TenantRoles
//	@Security		TenantID
//	@Security		UserID
//	@Param			from			query		string		false	"occurred_at lower bound (RFC 3339)"
//	@Param			to				query		string		false	"occurred_at upper bound (RFC 3339; default now)"
//	@Param			entry_type		query		[]string	false	"entry type (repeatable)"	collectionFormat(multi)
//	@Param			actor_id		query		string		false	"actor UUID"
//	@Param			actor_type		query		string		false	"user | service_account | iam_system | anonymous"
//	@Param			target_type		query		string		false	"target type"
//	@Param			target_id		query		string		false	"target id (requires target_type)"
//	@Param			source_service	query		string		false	"source service"
//	@Param			retention_tier	query		string		false	"compliance_7y | security_3y | access_90d"
//	@Param			limit			query		int			false	"page size (default 100, max 1000)"
//	@Param			cursor			query		string		false	"opaque keyset cursor"
//	@Success		200				{object}	EventsResponse
//	@Success		202				{object}	ExportAcceptedResponse	"large archived range deferred to an export"
//	@Failure		400				{object}	ErrorResponse			"invalid_request"
//	@Failure		401				{object}	ErrorResponse			"missing_identity_headers"
//	@Failure		403				{object}	ErrorResponse			"insufficient_permissions"
//	@Failure		429				{object}	ErrorResponse			"rate_limited (deferred export)"
//	@Failure		503				{object}	ErrorResponse			"dependency_unavailable"
//	@Router			/audit/events [get]
func (h *QueryHandler) ListEvents(c *gin.Context) {
	f, cursor, limit, err := parseQuery(c.Request.URL.Query())
	if err != nil {
		HandleError(c, err)
		return
	}
	tenantID, userID := identity(c)
	res, err := h.query.Query(c.Request.Context(), service.QueryRequest{TenantID: tenantID, Filter: f, Cursor: cursor, Limit: limit})
	if err != nil {
		HandleError(c, err)
		return
	}
	if res.Deferred != nil {
		if !h.allowExport(c, tenantID) {
			return
		}
		job, err := h.exports.Request(c.Request.Context(), tenantID, userID, *res.Deferred)
		if err != nil {
			HandleError(c, err)
			return
		}
		c.JSON(http.StatusAccepted, toExportAccepted(job))
		return
	}
	c.JSON(http.StatusOK, toEventsResponse(res))
}

// GetEvent is AL-2.
//
//	@Summary		Read one audit entry (AL-2)
//	@Description	Within the caller's tenant (RLS) and plan window. An entry whose month was archived is read from the tenant's archive object whose id range holds it (BUILD_PLAN gap 32).
//	@Tags			Audit
//	@Produce		json
//	@Security		TenantRoles
//	@Security		TenantID
//	@Security		UserID
//	@Param			id	path		string	true	"entry id (UUID)"
//	@Success		200	{object}	EventDTO
//	@Failure		400	{object}	ErrorResponse	"invalid_request"
//	@Failure		403	{object}	ErrorResponse	"insufficient_permissions"
//	@Failure		404	{object}	ErrorResponse	"audit_entry_not_found"
//	@Failure		503	{object}	ErrorResponse	"dependency_unavailable"
//	@Router			/audit/events/{id} [get]
func (h *QueryHandler) GetEvent(c *gin.Context) {
	tenantID, _ := identity(c)
	e, err := h.query.Get(c.Request.Context(), tenantID, c.Param("id"))
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, toEventDTO(e))
}

// CreateExport is AL-3.
//
//	@Summary		Request an asynchronous export (AL-3)
//	@Description	Body is the AL-1 filter (all optional), clamped to the plan window. Creates a pending job; poll AL-4. Rate-limited per tenant (§10.5).
//	@Tags			Audit
//	@Accept			json
//	@Produce		json
//	@Security		TenantRoles
//	@Security		TenantID
//	@Security		UserID
//	@Param			body	body		ExportFilterRequest	false	"filter"
//	@Success		202		{object}	ExportAcceptedResponse
//	@Failure		400		{object}	ErrorResponse	"invalid_request"
//	@Failure		403		{object}	ErrorResponse	"insufficient_permissions"
//	@Failure		429		{object}	ErrorResponse	"rate_limited"
//	@Failure		503		{object}	ErrorResponse	"dependency_unavailable"
//	@Router			/audit/exports [post]
func (h *QueryHandler) CreateExport(c *gin.Context) {
	var req ExportFilterRequest
	if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		abort(c, domain.ErrInvalidRequest, "request body is not a valid export filter")
		return
	}
	tenantID, userID := identity(c)
	if !h.allowExport(c, tenantID) {
		return
	}
	job, err := h.exports.Request(c.Request.Context(), tenantID, userID, req.filter())
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, toExportAccepted(job))
}

// GetExport is AL-4.
//
//	@Summary		Poll an export job (AL-4)
//	@Description	When ready, download_url is a freshly presigned short-lived URL (EXPORT_DOWNLOAD_URL_TTL) re-issued on every poll until expires_at (7 days, decision D-11).
//	@Tags			Audit
//	@Produce		json
//	@Security		TenantRoles
//	@Security		TenantID
//	@Security		UserID
//	@Param			id	path		string	true	"export id (UUID)"
//	@Success		200	{object}	ExportStatusResponse
//	@Failure		400	{object}	ErrorResponse	"invalid_request"
//	@Failure		403	{object}	ErrorResponse	"insufficient_permissions"
//	@Failure		404	{object}	ErrorResponse	"export_not_found"
//	@Failure		503	{object}	ErrorResponse	"dependency_unavailable"
//	@Router			/audit/exports/{id} [get]
func (h *QueryHandler) GetExport(c *gin.Context) {
	tenantID, _ := identity(c)
	v, err := h.exports.Status(c.Request.Context(), tenantID, c.Param("id"))
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, toExportStatus(v))
}

// InternalListEvents is AL-7.
//
//	@Summary		Mesh-only provenance read (AL-7)
//	@Description	For compliance/verification service callers (iam-system role). tenant_id is required and bound to app.tenant_id, so RLS still applies and there is no cross-tenant read. There is no plan-window clamp (§5.4); window_clamped is always false and effective_from is the lower bound applied. The filter, limit and cursor contract is the same as AL-1. An archived range over ARCHIVE_SYNC_MAX_ROWS / ARCHIVE_SYNC_MAX_BYTES returns 422 range_too_large; narrow from/to (decision D-14).
//	@Tags			Internal
//	@Produce		json
//	@Security		TenantRoles
//	@Security		TenantID
//	@Security		UserID
//	@Param			tenant_id		query		string		true	"tenant whose trail is read (UUID)"
//	@Param			from			query		string		false	"occurred_at lower bound (RFC 3339; default unbounded)"
//	@Param			to				query		string		false	"occurred_at upper bound (RFC 3339; default now)"
//	@Param			entry_type		query		[]string	false	"entry type (repeatable)"	collectionFormat(multi)
//	@Param			actor_id		query		string		false	"actor UUID"
//	@Param			actor_type		query		string		false	"user | service_account | iam_system | anonymous"
//	@Param			target_type		query		string		false	"target type"
//	@Param			target_id		query		string		false	"target id (requires target_type)"
//	@Param			source_service	query		string		false	"source service"
//	@Param			retention_tier	query		string		false	"compliance_7y | security_3y | access_90d"
//	@Param			limit			query		int			false	"page size (default 100, max 1000)"
//	@Param			cursor			query		string		false	"opaque keyset cursor"
//	@Success		200				{object}	EventsResponse
//	@Failure		400				{object}	ErrorResponse	"invalid_request"
//	@Failure		401				{object}	ErrorResponse	"missing_identity_headers"
//	@Failure		403				{object}	ErrorResponse	"forbidden_peer"
//	@Failure		422				{object}	ErrorResponse	"range_too_large"
//	@Failure		503				{object}	ErrorResponse	"dependency_unavailable"
//	@Router			/internal/audit/events [get]
func (h *QueryHandler) InternalListEvents(c *gin.Context) {
	q := c.Request.URL.Query()
	tenantID := q.Get("tenant_id")
	if !domain.IsUUID(tenantID) {
		abort(c, domain.ErrInvalidRequest, "tenant_id is required and must be a UUID")
		return
	}
	f, cursor, limit, err := parseQuery(q)
	if err != nil {
		HandleError(c, err)
		return
	}
	res, err := h.query.InternalQuery(c.Request.Context(), service.QueryRequest{TenantID: strings.ToLower(tenantID), Filter: f, Cursor: cursor, Limit: limit})
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, toEventsResponse(res))
}
