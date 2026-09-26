-- 000003_roles — LLD §4.3 / §10.4 roles and least-privilege grants.
--
--   audit_app        cmd/server. NOBYPASSRLS. INSERT+SELECT only on
--                    audit_events — never UPDATE/DELETE (AL-INV-1;
--                    asserted by .github/scripts/check-grants.sh and
--                    test/postgres grants tests).
--   audit_reconciler cmd/reconciler. BYPASSRLS. SELECT+DELETE on
--                    audit_events (partition lifecycle); archive-state and
--                    redaction-task bookkeeping. Partition DDL goes through
--                    SECURITY DEFINER functions (000005, Phase 7), since
--                    CREATE/DETACH/DROP PARTITION require table ownership.
--   audit_migrator   migrations. BYPASSRLS. Owns the schema.
--   admin_readonly   operator tooling. NOLOGIN BYPASSRLS, SELECT only;
--                    every session emits security.cross_tenant_access.
--
-- Decisions D-1/D-2 (BUILD_PLAN §C): audit_app additionally INSERTs
-- audit_redaction_tasks (immediate redaction is applied through a
-- reconciler-owned SECURITY DEFINER function, Phase 6) and SELECT/INSERT/
-- UPDATEs audit_export_jobs (the export worker runs in cmd/server, Phase 4).
--
-- Production roles are provisioned by Terraform (LOGIN + credentials); this
-- migration only creates them NOLOGIN when missing and the current user may
-- CREATE ROLE, re-asserts the RLS attributes, and attaches grants.
-- Idempotent and safe to re-run in every environment.

DO $$
DECLARE
    v_can_create boolean;
    r record;
BEGIN
    SELECT rolcreaterole OR rolsuper INTO v_can_create FROM pg_roles WHERE rolname = current_user;

    FOR r IN SELECT * FROM (VALUES
        ('audit_app',        'NOBYPASSRLS'),
        ('audit_reconciler', 'BYPASSRLS'),
        ('audit_migrator',   'BYPASSRLS'),
        ('admin_readonly',   'BYPASSRLS')
    ) AS t(name, rls)
    LOOP
        IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r.name) THEN
            IF v_can_create THEN
                EXECUTE format('CREATE ROLE %I NOLOGIN %s', r.name, r.rls);
            ELSE
                RAISE NOTICE 'role % missing and % lacks CREATEROLE — skipping (Terraform provisions it in prod)', r.name, current_user;
            END IF;
        ELSE
            BEGIN
                EXECUTE format('ALTER ROLE %I %s', r.name, r.rls);
            EXCEPTION WHEN insufficient_privilege THEN
                IF r.name = 'audit_app' AND EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'audit_app' AND rolbypassrls) THEN
                    RAISE WARNING 'audit_app has BYPASSRLS and % cannot strip it — AL-INV-3 VIOLATION, operator action required', current_user;
                ELSE
                    RAISE NOTICE 'cannot ALTER ROLE % as % — attribute refresh skipped', r.name, current_user;
                END IF;
            END;
        END IF;
    END LOOP;
END$$;

-- ── audit_app (cmd/server) ───────────────────────────────────────────────
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'audit_app') THEN
        RAISE NOTICE 'audit_app missing — grants skipped';
        RETURN;
    END IF;
    GRANT USAGE ON SCHEMA public TO audit_app;
    GRANT EXECUTE ON FUNCTION app_tenant_id()                     TO audit_app;
    GRANT EXECUTE ON FUNCTION log_rls_violation(text, uuid, text) TO audit_app;
    GRANT EXECUTE ON FUNCTION rls_check_tenant(uuid, text)        TO audit_app;

    GRANT SELECT, INSERT ON audit_events TO audit_app;                     -- AL-INV-1: exactly this
    GRANT SELECT, INSERT ON processed_events TO audit_app;                 -- dedup ledger (AL-INV-4)
    GRANT SELECT, INSERT, UPDATE ON tenant_plan_window TO audit_app;       -- recency-guarded projection
    GRANT SELECT, INSERT, UPDATE ON audit_export_jobs TO audit_app;        -- D-2 export worker
    GRANT INSERT ON audit_redaction_tasks TO audit_app;                    -- D-1 task insert on UserDeleted
END$$;

-- ── audit_reconciler (cmd/reconciler) ────────────────────────────────────
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'audit_reconciler') THEN
        RAISE NOTICE 'audit_reconciler missing — grants skipped';
        RETURN;
    END IF;
    GRANT USAGE ON SCHEMA public TO audit_reconciler;
    GRANT SELECT, DELETE ON audit_events TO audit_reconciler;               -- sole DELETE path (§10.4)
    GRANT SELECT, INSERT, UPDATE ON audit_event_archive_state TO audit_reconciler;
    GRANT SELECT, DELETE ON processed_events TO audit_reconciler;           -- 8-day prune
    GRANT SELECT, UPDATE ON audit_redaction_tasks TO audit_reconciler;      -- status progression / missed
END$$;

-- ── admin_readonly (operator tooling) ────────────────────────────────────
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'admin_readonly') THEN
        RETURN;
    END IF;
    GRANT USAGE ON SCHEMA public TO admin_readonly;
    GRANT SELECT ON audit_events, audit_event_archive_state, processed_events, tenant_plan_window,
                    audit_export_jobs, audit_redaction_tasks, rls_violation_log TO admin_readonly;
    COMMENT ON ROLE admin_readonly IS
        'Cross-tenant support reader. BYPASSRLS + SELECT only; every session emits security.cross_tenant_access (HLD §7.2).';
END$$;
