//go:build e2e

package e2e_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Producer contract tests: the exact request shapes the already-shipped AL-5
// clients send (LLD §18.2). A change here that breaks them breaks this
// suite, not their pending_audit_entries delivery queues.

// iam-realm-provisioner/internal/adapter/outbound/auditlog/client.go
// (AL-Q12, RP-6/RP-7 → config.idp.changed): x-tenant-id = the entry's
// tenant; x-user-id = the …00a1 sentinel; target omitted when nil.
func TestProducerContract_RealmProvisioner(t *testing.T) {
	e := newE2EEnv(t)
	body := `{"tenant_id":"` + tenantA + `","entry_type":"config.idp.changed","action":"create",
	  "actor":{"type":"iam_system","id":"00000000-0000-0000-0000-0000000000a1"},
	  "target":{"type":"idp","id":"okta-saml"},
	  "occurred_at":"2026-09-20T10:00:00.123456789Z","source_service":"iam-realm-provisioner",
	  "source_event_type":"TenantIdpConfigChanged","trace_id":"0af7651916cd43dd8448eb211c80319c",
	  "metadata":{"alias":"okta-saml","protocol":"saml"}}`
	h := map[string]string{
		"Content-Type": "application/json", "Idempotency-Key": uuid.NewString(),
		"x-user-id": systemActor, "x-tenant-id": tenantA, "x-tenant-roles": "iam-system",
	}
	code, _, resp := e.do(t, reqOpts{method: http.MethodPost, path: "/api/v1/internal/audit-entries", body: body, headers: h})
	require.Equal(t, http.StatusCreated, code, resp) // RP treats 200/201 as delivered
	assert.Equal(t, "security_3y", decodeEntry(t, resp).RetentionTier)

	code, _, _ = e.do(t, reqOpts{method: http.MethodPost, path: "/api/v1/internal/audit-entries", body: body, headers: h})
	assert.Equal(t, http.StatusOK, code, "RP's retry with the same key must be a 200 replay, never 409")
}

// iam-catalog-admin/internal/adapter/outbound/auditlog/client.go
// (CAT-1/2/5, AL-D14): body tenant_id AND x-tenant-id = platform_tenant;
// target always present; metadata {changes, record_version}.
func TestProducerContract_Catalog(t *testing.T) {
	e := newE2EEnv(t)
	for _, tc := range []struct{ entryType, srcType, targetType string }{
		{"config.department.created", "DepartmentCreated", "department"},
		{"config.department.updated", "DepartmentUpdated", "department"},
		{"config.plan.updated", "PlanUpdated", "plan"},
	} {
		body := `{"tenant_id":"` + platformTen + `","entry_type":"` + tc.entryType + `","action":"update",
		  "actor":{"type":"user","id":"` + adminUser + `"},
		  "target":{"type":"` + tc.targetType + `","id":"pro"},
		  "occurred_at":"2026-09-20T10:00:00Z","source_service":"iam-catalog-admin",
		  "source_event_type":"` + tc.srcType + `",
		  "metadata":{"changes":{"audit_query_window_days":{"from":1095,"to":1460}},"record_version":4}}`
		h := map[string]string{
			"Content-Type": "application/json", "Idempotency-Key": uuid.NewString(),
			"x-user-id": systemActor, "x-tenant-id": platformTen, "x-tenant-roles": "iam-system",
		}
		code, _, resp := e.do(t, reqOpts{method: http.MethodPost, path: "/api/v1/internal/audit-entries", body: body, headers: h})
		require.Equal(t, http.StatusCreated, code, "%s: %s", tc.entryType, resp)
		got := decodeEntry(t, resp)
		assert.Equal(t, platformTen, got.TenantID)
		assert.Equal(t, "security_3y", got.RetentionTier)
	}
}
