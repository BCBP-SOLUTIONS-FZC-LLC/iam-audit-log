package postgres

import "testing"

func TestCheckAppRole(t *testing.T) {
	if err := CheckAppRole(RoleInfo{Name: "audit_app"}, "audit_app"); err != nil {
		t.Fatalf("expected ok: %v", err)
	}
	if CheckAppRole(RoleInfo{Name: "audit_reconciler", BypassRLS: true}, "audit_app") == nil {
		t.Fatal("server must refuse to run as the reconciler role (rule 5)")
	}
	if CheckAppRole(RoleInfo{Name: "audit_app", BypassRLS: true}, "audit_app") == nil {
		t.Fatal("audit_app with BYPASSRLS must be refused (AL-INV-3)")
	}
}

func TestCheckReconcilerRole(t *testing.T) {
	if err := CheckReconcilerRole(RoleInfo{Name: "audit_reconciler", BypassRLS: true}, "audit_reconciler"); err != nil {
		t.Fatalf("expected ok: %v", err)
	}
	if CheckReconcilerRole(RoleInfo{Name: "audit_app"}, "audit_reconciler") == nil {
		t.Fatal("reconciler must refuse to run as the app role (rule 5)")
	}
	if CheckReconcilerRole(RoleInfo{Name: "audit_reconciler"}, "audit_reconciler") == nil {
		t.Fatal("reconciler without BYPASSRLS must be refused")
	}
}
