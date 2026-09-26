// Package requestctx carries the validated caller identity extracted from
// trusted gateway headers (x-user-id, x-tenant-id, x-tenant-roles) through
// the request scope. Handlers and services read this struct rather than
// reaching back into gin.Context, keeping core packages framework-free.
package requestctx

import (
	"context"
	"slices"

	"github.com/google/uuid"
)

type contextKey struct{}

// RequestContext holds the validated caller identity for a single request.
type RequestContext struct {
	UserID    uuid.UUID
	TenantID  uuid.UUID
	Roles     []string
	ClientIP  string
	UserAgent string
}

// WithContext returns a new context carrying rc.
func WithContext(ctx context.Context, rc *RequestContext) context.Context {
	return context.WithValue(ctx, contextKey{}, rc)
}

// FromContext retrieves the RequestContext set by the auth middleware.
func FromContext(ctx context.Context) (*RequestContext, bool) {
	rc, ok := ctx.Value(contextKey{}).(*RequestContext)
	return rc, ok && rc != nil
}

// HasRole reports whether the caller was granted the given role by the gateway.
func (rc *RequestContext) HasRole(role string) bool {
	return slices.Contains(rc.Roles, role)
}

// Role names this service authorizes against (LLD §5.2, §10.2).
const (
	RoleSystem      = "iam-system"
	RoleTenantAdmin = "tenant_admin"
	RoleTenantOwner = "tenant_owner"
)

// IsSystem reports whether the caller is a recognized mesh peer — the
// reserved iam-system principal every IAM producer presents on
// /api/v1/internal/* calls (LLD §10.2; x-user-id=…00a1,
// x-tenant-roles: iam-system).
func (rc *RequestContext) IsSystem() bool {
	return rc.HasRole(RoleSystem)
}

// IsAuditReader reports whether the caller may read the tenant's audit
// trail: tenant_admin or tenant_owner (LLD §5.2, HLD §6.4).
func (rc *RequestContext) IsAuditReader() bool {
	return rc.HasRole(RoleTenantAdmin) || rc.HasRole(RoleTenantOwner)
}
