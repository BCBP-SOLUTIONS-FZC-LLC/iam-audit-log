-- 000010_rls_violation_export — feeds iam_rls_violations_total (Tier 2,
-- domain-shared; LLD §4.3, §11; Enterprise Platform Observability Standard).
--
-- rls_check_tenant() samples violations into rls_violation_log, which
-- audit_app cannot read. audit_rls_violation_counts() returns the per-type
-- count of rows logged since the last call and advances a watermark in the
-- same statement, so every logged row is counted exactly once across the
-- whole cmd/server fleet (three replicas polling never triple-count). It is
-- SECURITY DEFINER, owned by the migrator, and returns aggregates only;
-- EXECUTE is granted to audit_app.
CREATE TABLE ops_export_watermark (
    name    text   PRIMARY KEY,
    last_id bigint NOT NULL
);
INSERT INTO ops_export_watermark (name, last_id) VALUES ('rls_violation_log', 0);
REVOKE ALL ON ops_export_watermark FROM PUBLIC;

CREATE OR REPLACE FUNCTION audit_rls_violation_counts()
RETURNS TABLE (violation_type text, violations bigint)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = public AS $$
DECLARE
    v_from bigint;
    v_to   bigint;
BEGIN
    SELECT w.last_id INTO v_from FROM ops_export_watermark w WHERE w.name = 'rls_violation_log' FOR UPDATE;
    SELECT max(l.id) INTO v_to FROM rls_violation_log l;
    IF v_to IS NULL OR v_to <= v_from THEN
        RETURN;
    END IF;
    UPDATE ops_export_watermark SET last_id = v_to WHERE name = 'rls_violation_log';
    RETURN QUERY
        SELECT l.violation_type, count(*)::bigint
          FROM rls_violation_log l
         WHERE l.id > v_from AND l.id <= v_to
         GROUP BY l.violation_type;
END;
$$;
REVOKE ALL ON FUNCTION audit_rls_violation_counts() FROM PUBLIC;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'audit_app') THEN
        GRANT EXECUTE ON FUNCTION audit_rls_violation_counts() TO audit_app;
    END IF;
END$$;
