//go:build e2e

package e2e_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func probeBody(t *testing.T, body string) (role, tenant string) {
	t.Helper()
	var out struct {
		DBRole   string `json:"db_role"`
		DBTenant string `json:"db_tenant"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &out), body)
	return out.DBRole, out.DBTenant
}

// AL-INV-3 end-to-end: the caller's x-tenant-id arrives in Postgres as the
// transaction-local app.tenant_id on the RLS-bound audit_app role — and a
// following request for another tenant sees only its own tenant.
func TestTenantHeaderReachesPostgresAsGUC_ALINV3(t *testing.T) {
	e := newE2EEnv(t)
	for _, tenant := range []string{tenantA, tenantB, tenantA} {
		code, _, body := e.do(t, reqOpts{path: "/api/v1/audit/probe", headers: adminHeaders(tenant)})
		require.Equal(t, http.StatusOK, code, body)
		role, got := probeBody(t, body)
		assert.Equal(t, "audit_app", role)
		assert.Equal(t, tenant, got)
	}
}

// LLD §5.2: only tenant_admin / tenant_owner may use /api/v1/audit/*.
func TestAuditRoutesRoleMatrix(t *testing.T) {
	e := newE2EEnv(t)
	cases := []struct {
		roles string
		want  int
		code  string
	}{
		{"tenant_admin", http.StatusOK, ""},
		{"tenant_owner", http.StatusOK, ""},
		{"tenant_admin,member", http.StatusOK, ""},
		{"member", http.StatusForbidden, "insufficient_permissions"},
		{"iam-system", http.StatusForbidden, "insufficient_permissions"},
	}
	for _, tc := range cases {
		code, _, body := e.do(t, reqOpts{path: "/api/v1/audit/probe", headers: gatewayHeaders(adminUser, tenantA, tc.roles)})
		assert.Equal(t, tc.want, code, tc.roles)
		if tc.code != "" {
			assert.Contains(t, body, `"code":"`+tc.code+`"`, tc.roles)
		}
	}
}

// LLD §10.2 / §17: /api/v1/internal/* admits only iam-system mesh peers;
// anyone else — including a tenant admin — is 403 forbidden_peer.
func TestInternalRoutesRequireMeshPeer(t *testing.T) {
	e := newE2EEnv(t)
	h := adminHeaders(tenantA)
	h["Idempotency-Key"] = "k1"
	code, _, body := e.do(t, reqOpts{method: http.MethodPost, path: "/api/v1/internal/probe", headers: h})
	assert.Equal(t, http.StatusForbidden, code)
	assert.Contains(t, body, `"code":"forbidden_peer"`)

	// The platform_tenant sentinel is an ordinary tenant to the ingest path
	// (AL-D14): it is bound as the GUC like any other tenant id.
	h = systemHeaders(platformTen)
	h["Idempotency-Key"] = "k2"
	code, _, body = e.do(t, reqOpts{method: http.MethodPost, path: "/api/v1/internal/probe", headers: h})
	require.Equal(t, http.StatusOK, code, body)
	_, tenant := probeBody(t, body)
	assert.Equal(t, platformTen, tenant)
}

// LLD §5.4: Idempotency-Key is required on direct-write — rejected, never minted.
func TestInternalWriteRequiresIdempotencyKey(t *testing.T) {
	e := newE2EEnv(t)
	code, _, body := e.do(t, reqOpts{method: http.MethodPost, path: "/api/v1/internal/probe", headers: systemHeaders(tenantA)})
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Contains(t, body, `"code":"invalid_request"`)
}

func TestMissingOrMalformedIdentity401(t *testing.T) {
	e := newE2EEnv(t)
	for _, h := range []map[string]string{
		nil,
		{"x-user-id": adminUser}, // no tenant
		gatewayHeaders(adminUser, "not-a-uuid", "tenant_admin"),
		gatewayHeaders("not-a-uuid", tenantA, "tenant_admin"),
	} {
		code, _, _ := e.do(t, reqOpts{path: "/api/v1/audit/probe", headers: h})
		assert.Equal(t, http.StatusUnauthorized, code, h)
	}
}

func TestContentTypeGate(t *testing.T) {
	e := newE2EEnv(t)
	h := systemHeaders(tenantA)
	h["Idempotency-Key"] = "k3"
	h["Content-Type"] = "text/plain"
	code, _, _ := e.do(t, reqOpts{method: http.MethodPost, path: "/api/v1/internal/probe", body: "x", headers: h})
	assert.Equal(t, http.StatusBadRequest, code)
}
