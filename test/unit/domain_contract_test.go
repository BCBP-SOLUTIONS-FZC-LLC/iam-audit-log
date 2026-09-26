package unit_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/pkg/requestctx"
)

// LLD §10.3 / §25: reserved sentinels are frozen, well-formed UUIDs.
func TestSentinels_FrozenValues(t *testing.T) {
	cases := map[string]string{
		"iam-system":      domain.IAMSystemActorID,
		"platform_tenant": domain.PlatformTenantID,
	}
	want := map[string]string{
		"iam-system":      "00000000-0000-0000-0000-0000000000a1",
		"platform_tenant": "00000000-0000-0000-0000-0000000000b1",
	}
	for k, v := range cases {
		if v != want[k] {
			t.Errorf("%s = %s, want %s", k, v, want[k])
		}
		if _, err := uuid.Parse(v); err != nil {
			t.Errorf("%s is not a UUID: %v", k, err)
		}
	}
}

// LLD §17: the full error taxonomy, black-box.
func TestErrorTaxonomy_LLDTable(t *testing.T) {
	table := map[string]int{
		"invalid_request": 400, "insufficient_permissions": 403, "forbidden_peer": 403,
		"audit_entry_not_found": 404, "export_not_found": 404,
		"unknown_entry_type": 422, "invalid_actor": 422, "metadata_too_large": 422, "batch_too_large": 422,
		"rate_limited": 429, "dependency_unavailable": 503,
	}
	for code, status := range table {
		if got := domain.NewError(domain.ErrorCode(code), "x").Status(); got != status {
			t.Errorf("%s → %d, want %d (LLD §17)", code, got, status)
		}
	}
}

// LLD §5.2: role predicates the route guards rely on.
func TestRequestContext_RolePredicates(t *testing.T) {
	cases := []struct {
		roles          []string
		system, reader bool
	}{
		{[]string{"iam-system"}, true, false},
		{[]string{"tenant_admin"}, false, true},
		{[]string{"tenant_owner"}, false, true},
		{[]string{"member"}, false, false},
		{nil, false, false},
	}
	for _, tc := range cases {
		rc := &requestctx.RequestContext{Roles: tc.roles}
		if rc.IsSystem() != tc.system || rc.IsAuditReader() != tc.reader {
			t.Errorf("roles %v: system=%v reader=%v", tc.roles, rc.IsSystem(), rc.IsAuditReader())
		}
	}
	if _, ok := requestctx.FromContext(requestctx.WithContext(t.Context(), nil)); ok {
		t.Error("a nil RequestContext must not be reported as present")
	}
}
