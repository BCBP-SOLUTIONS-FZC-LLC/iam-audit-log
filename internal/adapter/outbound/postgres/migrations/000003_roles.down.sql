-- Reverses 000003_roles: revokes every grant it attached. Roles themselves
-- are left in place — they are cluster-wide objects (and Terraform-owned in
-- production), not per-database schema.
DO $$
DECLARE r text;
BEGIN
    FOREACH r IN ARRAY ARRAY['audit_app', 'audit_reconciler', 'admin_readonly'] LOOP
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('REVOKE ALL ON audit_events, audit_event_archive_state, processed_events, tenant_plan_window, audit_export_jobs, audit_redaction_tasks, rls_violation_log FROM %I', r);
            EXECUTE format('REVOKE ALL ON FUNCTION app_tenant_id(), log_rls_violation(text, uuid, text), rls_check_tenant(uuid, text) FROM %I', r);
            EXECUTE format('REVOKE USAGE ON SCHEMA public FROM %I', r);
        END IF;
    END LOOP;
END$$;
