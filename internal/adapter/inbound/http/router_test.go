package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/pkg/requestctx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	_ = gincommon.ObservabilityMiddlewares(gincommon.Config{ServiceName: "iam-audit-log", BuildVersion: "test"})
	os.Exit(m.Run())
}

type pingerFn func(context.Context) error

func (f pingerFn) Health(ctx context.Context) error { return f(ctx) }

const (
	tenantA = "11111111-1111-1111-1111-111111111111"
	userA   = "22222222-2222-2222-2222-222222222222"
)

// probeRouter mounts one probe route in each protected group that echoes
// the bound request context + GUC so middleware behavior is observable.
func probeRouter(ready map[string]Pinger) *Router {
	echo := func(c *gin.Context) {
		rc, _ := requestctx.FromContext(c.Request.Context())
		g, _ := pgcommon.GUCSetFromContext(c.Request.Context())
		c.JSON(http.StatusOK, gin.H{"tenant": rc.TenantID.String(), "user": rc.UserID.String(), "guc_tenant": g.TenantID})
	}
	return NewRouter(RouterConfig{
		GinConfig: gincommon.Config{ServiceName: "iam-audit-log", BuildVersion: "test"},
		Docs:      DocsConfig{Environment: "test"},
		Ready:     ready,
		Audit:     func(g *gin.RouterGroup) { g.GET("/probe", echo) },
		Internal: func(g *gin.RouterGroup) {
			g.GET("/probe", echo)
			g.POST("/idem", RequireIdempotencyKey(), echo)
		},
	})
}

func do(t *testing.T, r *Router, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.Handler().ServeHTTP(w, req)
	return w
}

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out), w.Body.String())
	return out
}

func TestHealthz_AlwaysOK(t *testing.T) {
	w := do(t, probeRouter(nil), http.MethodGet, "/healthz", nil)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestReadyz_ReportsEachCheck(t *testing.T) {
	ok := pingerFn(func(context.Context) error { return nil })
	down := pingerFn(func(context.Context) error { return errors.New("down") })

	w := do(t, probeRouter(map[string]Pinger{"database": ok}), http.MethodGet, "/readyz", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	w = do(t, probeRouter(map[string]Pinger{"database": ok, "consumers": down}), http.MethodGet, "/readyz", nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	checks := decode(t, w)["checks"].(map[string]any)
	assert.Equal(t, "ok", checks["database"])
	assert.Equal(t, "down", checks["consumers"])
}

func TestAsyncAPI_ServedBeforeAuth(t *testing.T) {
	r := probeRouter(nil)
	assert.Equal(t, http.StatusOK, do(t, r, http.MethodGet, "/asyncapi.yaml", nil).Code)
	assert.Equal(t, http.StatusOK, do(t, r, http.MethodGet, "/asyncapi", nil).Code)
}

func TestProtected_MissingIdentityHeaders_401(t *testing.T) {
	r := probeRouter(nil)
	assert.Equal(t, http.StatusUnauthorized, do(t, r, http.MethodGet, "/api/v1/audit/probe", nil).Code)
	assert.Equal(t, http.StatusUnauthorized, do(t, r, http.MethodGet, "/api/v1/internal/probe", nil).Code)
}

func TestProtected_MalformedTenantHeader_401(t *testing.T) {
	w := do(t, probeRouter(nil), http.MethodGet, "/api/v1/audit/probe", map[string]string{
		"x-user-id": userA, "x-tenant-id": "not-a-uuid", "x-tenant-roles": "tenant_admin",
	})
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, "missing_identity_headers", decode(t, w)["code"])
}

// LLD §5.2: only tenant_admin / tenant_owner may read the audit trail.
func TestAuditRoutes_RequireAuditReader(t *testing.T) {
	r := probeRouter(nil)
	for _, role := range []string{"tenant_admin", "tenant_owner"} {
		w := do(t, r, http.MethodGet, "/api/v1/audit/probe", map[string]string{
			"x-user-id": userA, "x-tenant-id": tenantA, "x-tenant-roles": role,
		})
		assert.Equal(t, http.StatusOK, w.Code, role)
	}
	w := do(t, r, http.MethodGet, "/api/v1/audit/probe", map[string]string{
		"x-user-id": userA, "x-tenant-id": tenantA, "x-tenant-roles": "member",
	})
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, "insufficient_permissions", decode(t, w)["code"])
}

// AL-INV-3: the caller's x-tenant-id is bound as the transaction-local
// app.tenant_id GUC set for every subsequent pgcommon checkout.
func TestIdentityBridge_BindsTenantGUC_ALINV3(t *testing.T) {
	w := do(t, probeRouter(nil), http.MethodGet, "/api/v1/audit/probe", map[string]string{
		"x-user-id": userA, "x-tenant-id": tenantA, "x-tenant-roles": "tenant_admin",
	})
	require.Equal(t, http.StatusOK, w.Code)
	body := decode(t, w)
	assert.Equal(t, tenantA, body["tenant"])
	assert.Equal(t, tenantA, body["guc_tenant"])
}

// LLD §10.2, §17: an unrecognized mesh peer on /internal/* is 403 forbidden_peer.
func TestInternalRoutes_RequireSystemRole(t *testing.T) {
	r := probeRouter(nil)
	w := do(t, r, http.MethodGet, "/api/v1/internal/probe", map[string]string{
		"x-user-id": userA, "x-tenant-id": tenantA, "x-tenant-roles": "tenant_admin",
	})
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, "forbidden_peer", decode(t, w)["code"])

	// Both sibling spellings of the iam-system principal are accepted.
	for _, uid := range []string{"00000000-0000-0000-0000-0000000000a1", "iam-system"} {
		w = do(t, r, http.MethodGet, "/api/v1/internal/probe", map[string]string{
			"x-user-id": uid, "x-tenant-id": "00000000-0000-0000-0000-0000000000b1", "x-tenant-roles": "iam-system",
		})
		assert.Equal(t, http.StatusOK, w.Code, uid)
		assert.Equal(t, "00000000-0000-0000-0000-000000000000", decode(t, w)["user"])
	}
}

// LLD §5.4: Idempotency-Key is required on direct-write — rejected, never minted.
func TestRequireIdempotencyKey(t *testing.T) {
	r := probeRouter(nil)
	sys := map[string]string{"x-user-id": "iam-system", "x-tenant-id": tenantA, "x-tenant-roles": "iam-system"}
	w := do(t, r, http.MethodPost, "/api/v1/internal/idem", sys)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "invalid_request", decode(t, w)["code"])

	sys["Idempotency-Key"] = "k-1"
	assert.Equal(t, http.StatusOK, do(t, r, http.MethodPost, "/api/v1/internal/idem", sys).Code)

	long := make([]byte, 257)
	for i := range long {
		long[i] = 'a'
	}
	sys["Idempotency-Key"] = string(long)
	assert.Equal(t, http.StatusBadRequest, do(t, r, http.MethodPost, "/api/v1/internal/idem", sys).Code)
}

func TestDocsHiddenInProductionUnlessEnabled(t *testing.T) {
	r := NewRouter(RouterConfig{
		GinConfig: gincommon.Config{ServiceName: "iam-audit-log", BuildVersion: "test"},
		Docs:      DocsConfig{Environment: "production"},
	})
	assert.Equal(t, http.StatusNotFound, do(t, r, http.MethodGet, "/asyncapi.yaml", nil).Code)

	r = NewRouter(RouterConfig{
		GinConfig: gincommon.Config{ServiceName: "iam-audit-log", BuildVersion: "test"},
		Docs:      DocsConfig{Environment: "production", Enabled: true, AuthToken: "s3cret"},
	})
	assert.Equal(t, http.StatusUnauthorized, do(t, r, http.MethodGet, "/asyncapi.yaml", nil).Code)
	assert.Equal(t, http.StatusOK, do(t, r, http.MethodGet, "/asyncapi.yaml", map[string]string{"Authorization": "Bearer s3cret"}).Code)
}
