-- 000002_rls — LLD §4.3 Row-Level Security (AL-INV-3: fail-closed).
-- RLS is enforced on the partitioned parent; queries through audit_events
-- apply it to every partition, and audit_app holds no privilege on any
-- partition directly (see test/postgres partition tests).

-- ─────────────────────────────────────────────────────────────────────────
-- app_tenant_id() — reads the transaction-local app.tenant_id GUC that
-- pgcommon binds (set_config(…, true)). Fail-closed: a missing or
-- malformed value returns NULL, so rls_check_tenant() denies.
-- ─────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION app_tenant_id() RETURNS uuid
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = public AS $$
DECLARE v text;
BEGIN
    v := current_setting('app.tenant_id', true);
    IF v IS NULL OR v = '' THEN RETURN NULL; END IF;
    RETURN v::uuid;
EXCEPTION WHEN OTHERS THEN RETURN NULL;   -- malformed GUC → NULL → zero rows
END;
$$;

-- ─────────────────────────────────────────────────────────────────────────
-- rls_violation_log / log_rls_violation() — 1%-sampled trail feeding
-- iam_rls_violations_total (§4.3, §11). RLS stays disabled on the log
-- itself (anti-recursion); the logger swallows its own errors so it can
-- never abort the caller's statement.
-- ─────────────────────────────────────────────────────────────────────────
CREATE TABLE rls_violation_log (
    id               bigserial   PRIMARY KEY,
    table_name       text        NOT NULL,
    row_tenant_id    uuid,
    app_tenant_id    uuid,
    violation_type   text        NOT NULL,   -- 'missing_or_invalid_guc' | 'cross_tenant_access'
    session_role     text        DEFAULT SESSION_USER,
    client_addr      inet        DEFAULT inet_client_addr(),
    application_name text        DEFAULT current_setting('application_name', true),
    query_text       text,
    occurred_at      timestamptz NOT NULL DEFAULT now()
);

CREATE OR REPLACE FUNCTION log_rls_violation(
    p_table_name     text,
    p_row_tenant_id  uuid,
    p_violation_type text
) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = public AS $$
BEGIN
    IF random() > 0.01 THEN RETURN; END IF;
    INSERT INTO rls_violation_log (table_name, row_tenant_id, app_tenant_id, violation_type, query_text)
    VALUES (p_table_name, p_row_tenant_id, app_tenant_id(), p_violation_type, current_query());
EXCEPTION WHEN OTHERS THEN
    NULL;
END;
$$;

-- ─────────────────────────────────────────────────────────────────────────
-- rls_check_tenant() — true iff the row's tenant_id equals app.tenant_id.
-- Identical contract to the sibling services.
-- ─────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION rls_check_tenant(p_tenant_id uuid, p_table_name text)
RETURNS boolean
    LANGUAGE plpgsql STABLE STRICT SECURITY DEFINER
    SET search_path = public AS $$
DECLARE v_app uuid;
BEGIN
    v_app := app_tenant_id();
    IF v_app IS NULL THEN
        PERFORM log_rls_violation(p_table_name, p_tenant_id, 'missing_or_invalid_guc');
        RETURN false;                          -- fail-closed
    END IF;
    IF p_tenant_id <> v_app THEN
        PERFORM log_rls_violation(p_table_name, p_tenant_id, 'cross_tenant_access');
        RETURN false;
    END IF;
    RETURN true;
END;
$$;

-- SECURITY DEFINER functions are not callable by PUBLIC; 000003_roles
-- grants EXECUTE to the roles that need it.
REVOKE ALL ON FUNCTION app_tenant_id()                      FROM PUBLIC;
REVOKE ALL ON FUNCTION log_rls_violation(text, uuid, text)  FROM PUBLIC;
REVOKE ALL ON FUNCTION rls_check_tenant(uuid, text)         FROM PUBLIC;

-- ─────────────────────────────────────────────────────────────────────────
-- Tenant-scoped tables: ENABLE + FORCE (applies even to the owner) +
-- REVOKE from PUBLIC + tenant_isolation USING/WITH CHECK. WITH CHECK is
-- what makes direct-write cross-tenant forgery structurally impossible
-- (§4.3, §10.2).
-- ─────────────────────────────────────────────────────────────────────────
ALTER TABLE audit_events      ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_export_jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_events      FORCE  ROW LEVEL SECURITY;
ALTER TABLE audit_export_jobs FORCE  ROW LEVEL SECURITY;
REVOKE ALL ON audit_events      FROM PUBLIC;
REVOKE ALL ON audit_export_jobs FROM PUBLIC;

CREATE POLICY tenant_isolation ON audit_events
    USING      (rls_check_tenant(tenant_id, 'audit_events'))
    WITH CHECK (rls_check_tenant(tenant_id, 'audit_events'));

CREATE POLICY tenant_isolation ON audit_export_jobs
    USING      (rls_check_tenant(tenant_id, 'audit_export_jobs'))
    WITH CHECK (rls_check_tenant(tenant_id, 'audit_export_jobs'));

-- The RLS-exempt tables (§4.6) are still closed to PUBLIC.
REVOKE ALL ON audit_event_archive_state, processed_events, tenant_plan_window,
              audit_redaction_tasks, rls_violation_log FROM PUBLIC;
