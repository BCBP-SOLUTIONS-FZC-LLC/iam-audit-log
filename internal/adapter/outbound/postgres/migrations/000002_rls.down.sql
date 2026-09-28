-- Reverses 000002_rls.
DROP POLICY IF EXISTS tenant_isolation ON audit_export_jobs;
DROP POLICY IF EXISTS tenant_isolation ON audit_events;
ALTER TABLE audit_export_jobs NO FORCE ROW LEVEL SECURITY;
ALTER TABLE audit_events      NO FORCE ROW LEVEL SECURITY;
ALTER TABLE audit_export_jobs DISABLE ROW LEVEL SECURITY;
ALTER TABLE audit_events      DISABLE ROW LEVEL SECURITY;

DROP FUNCTION IF EXISTS rls_check_tenant(uuid, text);
DROP FUNCTION IF EXISTS log_rls_violation(text, uuid, text);
DROP TABLE IF EXISTS rls_violation_log;
DROP FUNCTION IF EXISTS app_tenant_id();
