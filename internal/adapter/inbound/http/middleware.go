package http

import (
	"context"
	"net/http"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/pkg/requestctx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// IdempotencyKeyHeader is the required direct-write header (LLD §5.4, §25).
const IdempotencyKeyHeader = "Idempotency-Key"

// maxIdempotencyKeyLen bounds the key (it becomes source_event_id and the
// processed_events.event_id); matches the siblings' 256-char limit.
const maxIdempotencyKeyLen = 256

// IdentityBridgeMiddleware runs after gincommon.ProtectedMiddlewares. It
// parses the trusted gateway/mesh identity (x-user-id / x-tenant-id /
// x-tenant-roles — LLD §5.1, §10.2) into a typed requestctx.RequestContext
// and binds the RLS GUC set onto the request context, so every pgcommon
// checkout issues set_config('app.tenant_id', …, true) — transaction-local,
// never session-level (AL-INV-3).
//
// The reserved iam-system principal may present x-user-id as the sentinel
// UUID …00a1 or the literal "iam-system" (both used by sibling callers);
// either maps to uuid.Nil in the request context. The commercial-system
// principal may present …00c1 or the literal "commercial-system"; either maps
// to …00c1 (CD-19, D-25).
//
// Routes that bind a tenant other than the caller's x-tenant-id (AL-5's
// body tenant_id, AL-7's explicit tenant_id) re-bind the GUC in the
// handler via BindTenantGUC.
func IdentityBridgeMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		prc, ok := gincommon.RequestContext(c)
		if !ok {
			abort(c, domain.ErrUnauthenticated, "missing identity headers")
			return
		}
		var userID uuid.UUID
		switch prc.UserID {
		case domain.IAMSystemActorID, requestctx.RoleSystem:
			userID = uuid.Nil
		case requestctx.RoleCommercialSystem:
			userID = uuid.MustParse(domain.CommercialSystemActorID)
		default:
			id, err := uuid.Parse(prc.UserID)
			if err != nil {
				abort(c, domain.ErrUnauthenticated, "x-user-id header is not a valid UUID")
				return
			}
			userID = id
		}
		tenantID, err := uuid.Parse(prc.TenantID)
		if err != nil {
			abort(c, domain.ErrUnauthenticated, "x-tenant-id header is not a valid UUID")
			return
		}
		rc := &requestctx.RequestContext{
			UserID: userID, TenantID: tenantID, Roles: prc.Roles,
			ClientIP: prc.ClientIP, UserAgent: c.Request.Header.Get("User-Agent"),
		}
		ctx := requestctx.WithContext(c.Request.Context(), rc)
		ctx = BindTenantGUC(ctx, tenantID)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// BindTenantGUC returns ctx with app.tenant_id bound to tenantID for every
// subsequent pgcommon checkout (transaction-local set_config).
func BindTenantGUC(ctx context.Context, tenantID uuid.UUID) context.Context {
	g, _ := pgcommon.GUCSetFromContext(ctx)
	g.TenantID = tenantID.String()
	return pgcommon.WithGUCSet(ctx, g)
}

// RequireSystemRole gates the internal routes other than direct-write (AL-7)
// to callers presenting the iam-system role (LLD §5.2, §10.2). The mesh
// AuthorizationPolicy + NetworkPolicy are the primary control; this is
// defense-in-depth. An unrecognized peer is 403 forbidden_peer (§17).
func RequireSystemRole() gin.HandlerFunc {
	return requireRole(func(rc *requestctx.RequestContext) bool { return rc.IsSystem() })
}

// RequireDirectWriterRole gates AL-5/AL-6: the iam-system role or the
// commercial-system role (CD-19, D-25). Anything else is 403 forbidden_peer.
func RequireDirectWriterRole() gin.HandlerFunc {
	return requireRole(func(rc *requestctx.RequestContext) bool { return rc.IsDirectWriter() })
}

func requireRole(allowed func(*requestctx.RequestContext) bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		rc, ok := requestctx.FromContext(c.Request.Context())
		if !ok || !allowed(rc) {
			abort(c, domain.ErrForbiddenPeer, "internal route requires a recognized mesh peer identity")
			return
		}
		c.Next()
	}
}

// RequireAuditReader gates /api/v1/audit/* to tenant_admin / tenant_owner
// (LLD §5.2). A plain member is 403 insufficient_permissions.
func RequireAuditReader() gin.HandlerFunc {
	return func(c *gin.Context) {
		rc, ok := requestctx.FromContext(c.Request.Context())
		if !ok || !rc.IsAuditReader() {
			abort(c, domain.ErrInsufficientPermissions, "audit log access requires tenant_admin or tenant_owner")
			return
		}
		c.Next()
	}
}

// RequireIdempotencyKey enforces the required Idempotency-Key header on AL-5
// (LLD §5.4). A missing key is rejected — never silently minted — because the
// caller's stable key is what makes its retry-until-acked delivery safe
// (§18.2).
func RequireIdempotencyKey() gin.HandlerFunc {
	return func(c *gin.Context) {
		key := c.GetHeader(IdempotencyKeyHeader)
		if key == "" || len(key) > maxIdempotencyKeyLen {
			abort(c, domain.ErrInvalidRequest, "Idempotency-Key header is required (1..256 chars)")
			return
		}
		c.Next()
	}
}

// OptionalIdempotencyKey is AL-6's header check (D-26, superseding D-4): the
// header is optional, because each entry's idempotency_key is the dedup key;
// a header that is sent must still be 1..256 chars. It identifies the batch
// call only and is never stored.
func OptionalIdempotencyKey() gin.HandlerFunc {
	return func(c *gin.Context) {
		if v, sent := c.Request.Header[IdempotencyKeyHeader]; sent && (len(v) == 0 || v[0] == "" || len(v[0]) > maxIdempotencyKeyLen) {
			abort(c, domain.ErrInvalidRequest, "Idempotency-Key header, when sent, must be 1..256 chars")
			return
		}
		c.Next()
	}
}

// RequireJSONContentType rejects POST/PUT/PATCH bodies that are not
// application/json (LLD §5.1).
func RequireJSONContentType() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch:
		default:
			c.Next()
			return
		}
		if c.Request.ContentLength != 0 && c.ContentType() != "application/json" {
			abort(c, domain.ErrInvalidRequest, "Content-Type must be application/json")
			return
		}
		c.Next()
	}
}
