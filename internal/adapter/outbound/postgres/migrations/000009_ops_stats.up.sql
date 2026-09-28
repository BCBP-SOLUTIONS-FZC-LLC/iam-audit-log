-- 000009_ops_stats — Phase 8 (LLD §11, §20; decision D-21, gaps 40/41).
--
-- audit_ops_stats(): the DB-derived operational state that cmd/server (always
-- scraped) exports as gauges, so the Critical archival and redaction alerts do
-- not depend on scraping a short-lived reconciler CronJob. It returns
-- aggregate numbers only (no tenant ids, no rows). It is SECURITY DEFINER,
-- owned by the migrator, because audit_app has no read on the reconciler-only
-- tables or on audit_events_default outside RLS. EXECUTE is granted to
-- audit_app.
--
-- Eligibility matches domain.ArchiveEligible: a month is due once its end is
-- older than the hot window AND it is before the trailing writable months,
-- i.e. from greatest(month_end + hot_days, month + (trailing+1) months).
CREATE OR REPLACE FUNCTION audit_ops_stats(p_hot_days int, p_trailing int, p_grace interval, p_pending_age interval)
RETURNS TABLE (default_rows bigint, stalled_partitions bigint, archive_lag_seconds double precision, pending_redactions bigint)
    LANGUAGE plpgsql SECURITY DEFINER STABLE
    SET search_path = public AS $$
BEGIN
    IF p_hot_days IS NULL OR p_hot_days < 1 OR p_trailing IS NULL OR p_trailing < 0 OR p_trailing > 24
       OR p_grace IS NULL OR p_grace < interval '0' OR p_pending_age IS NULL OR p_pending_age < interval '0' THEN
        RAISE EXCEPTION 'audit_ops_stats: invalid parameters' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    RETURN QUERY
    WITH parts AS (
        SELECT to_date(substr(c.relname, 14), 'YYYY_MM') AS month
          FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
         WHERE i.inhparent = 'audit_events'::regclass
           AND c.relname ~ '^audit_events_[0-9]{4}_[0-9]{2}$'
    ), due AS (
        SELECT greatest((month + interval '1 month') + make_interval(days => p_hot_days),
                        month + make_interval(months => p_trailing + 1))::timestamp AT TIME ZONE 'UTC' AS eligible_at
          FROM parts
    )
    SELECT (SELECT count(*) FROM audit_events_default),
           (SELECT count(*) FROM due WHERE eligible_at + p_grace <= now()),
           coalesce((SELECT extract(epoch FROM now() - min(eligible_at))::double precision
                       FROM due WHERE eligible_at <= now()), 0),
           (SELECT count(*) FROM audit_redaction_tasks
             WHERE status = 'pending' AND requested_at <= now() - p_pending_age);
END;
$$;
REVOKE ALL ON FUNCTION audit_ops_stats(int, int, interval, interval) FROM PUBLIC;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'audit_app') THEN
        GRANT EXECUTE ON FUNCTION audit_ops_stats(int, int, interval, interval) TO audit_app;
    END IF;
END$$;
