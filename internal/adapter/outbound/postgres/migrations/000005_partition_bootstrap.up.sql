-- 000005_partition_bootstrap — LLD §4.2 / §4.4 / §19.
--
-- audit_ensure_partitions() creates any missing monthly partition
-- audit_events_YYYY_MM (UTC month bounds) from p_trailing months before the
-- current month to p_ahead months after it. PartitionService calls it at
-- server startup (defensively — a missed CronJob run never blocks
-- ingestion) and from the reconciler CronJob (§4.4).
--
-- Why SECURITY DEFINER: CREATE TABLE … PARTITION OF requires ownership of
-- audit_events, which no grant can confer; audit_app has no DDL rights and
-- cmd/server must never hold the reconciler role (implementation rule 5).
-- The function is owned by the migrating role and does exactly one bounded
-- thing — create empty partitions for a small window — so exposing it to
-- the runtime roles adds no ability to read, change, or remove audit data
-- (same narrow-definer pattern as decisions D-1/D-2).
--
-- A month whose range already has rows sitting in audit_events_default
-- cannot be created (PostgreSQL rejects the new partition bound); that
-- month is reported 'skipped_default_has_rows' rather than failing startup
-- — the operator re-homes the rows per RB-3.
CREATE OR REPLACE FUNCTION audit_ensure_partitions(p_ahead int, p_trailing int)
RETURNS TABLE (partition_name text, range_start timestamptz, range_end timestamptz, action text)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = public AS $$
DECLARE
    v_base  date;
    v_month date;
    i       int;
BEGIN
    IF p_ahead IS NULL OR p_ahead < 0 OR p_ahead > 24
       OR p_trailing IS NULL OR p_trailing < 0 OR p_trailing > 24 THEN
        RAISE EXCEPTION 'audit_ensure_partitions: window out of bounds (ahead=%, trailing=%; each must be 0..24)', p_ahead, p_trailing
            USING ERRCODE = 'invalid_parameter_value';
    END IF;

    v_base := date_trunc('month', now() AT TIME ZONE 'UTC')::date;
    FOR i IN -p_trailing .. p_ahead LOOP
        v_month        := (v_base + make_interval(months => i))::date;
        partition_name := 'audit_events_' || to_char(v_month, 'YYYY_MM');
        range_start    := v_month::timestamp AT TIME ZONE 'UTC';
        range_end      := (v_month + interval '1 month')::timestamp AT TIME ZONE 'UTC';

        IF to_regclass('public.' || partition_name) IS NOT NULL THEN
            action := 'exists';
        ELSE
            BEGIN
                EXECUTE format(
                    'CREATE TABLE %I PARTITION OF audit_events FOR VALUES FROM (%L) TO (%L)',
                    partition_name,
                    to_char(range_start AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS') || '+00',
                    to_char(range_end   AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS') || '+00');
                action := 'created';
            EXCEPTION
                WHEN duplicate_table THEN
                    action := 'exists';                       -- a concurrent replica won the race
                WHEN check_violation THEN
                    action := 'skipped_default_has_rows';     -- RB-3: re-home default-partition rows first
            END;
        END IF;
        RETURN NEXT;
    END LOOP;
END;
$$;

REVOKE ALL ON FUNCTION audit_ensure_partitions(int, int) FROM PUBLIC;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'audit_app') THEN
        GRANT EXECUTE ON FUNCTION audit_ensure_partitions(int, int) TO audit_app;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'audit_reconciler') THEN
        GRANT EXECUTE ON FUNCTION audit_ensure_partitions(int, int) TO audit_reconciler;
    END IF;
END$$;

-- Bootstrap: current month + 3 ahead (AUDIT_PRECREATE_MONTHS default) and
-- 3 trailing writable months (AUDIT_WRITABLE_TRAILING_MONTHS default).
SELECT count(*) FROM audit_ensure_partitions(3, 3);
