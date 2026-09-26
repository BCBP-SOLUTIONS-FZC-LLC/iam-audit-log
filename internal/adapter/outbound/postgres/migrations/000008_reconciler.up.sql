-- 000008_reconciler — Phase 7 (LLD §8.5, §8.6, §15.4, AL-D4; AL-INV-9,
-- AL-INV-12; decisions D-12, D-17, D-19, D-20).
--
-- 1. audit_archive_objects.sealed (D-20): set when the object's partition is
--    dropped, at which point its rows exist only in S3. Sealed objects are
--    immutable; unsealed ones (partition still present) are rewritten by a
--    re-archive (D-20). The query path and redaction's `missed` check now
--    route on `sealed` rather than partition existence (D-12, refined). A
--    re-opened month (D-19) has a partition again, yet its earlier rows
--    still live only in its sealed objects.
-- 2. apply_redaction() / sweep_redactions() also invalidate the
--    security_3y archive of every partition whose rows they rewrote, so an
--    object uploaded before a redaction is re-archived and never sealed
--    with stale PII.
-- 3. audit_drop_partition(name): the AL-INV-9 gate, in the database. Under
--    an ACCESS EXCLUSIVE lock on audit_events it requires both retained
--    tiers `verified`, live per-tier row counts equal to the unsealed
--    manifest (a late arrival → count_mismatch → re-open, AL-D4), and no
--    pending redaction task touching the partition (AL-INV-12). It then
--    seals the manifest, records the state, and DETACHes + DROPs.
-- 4. audit_reopen_partition(name) (D-19): re-creates a dropped month whose
--    late rows landed in audit_events_default, moves them in, and resets
--    its retained tiers to pending for re-archival.
-- Both partition functions are SECURITY DEFINER, owned by the migrating
-- role (partition DDL needs ownership of audit_events, gap 26), EXECUTE for
-- audit_reconciler only.

ALTER TABLE audit_archive_objects ADD COLUMN sealed boolean NOT NULL DEFAULT false;
UPDATE audit_archive_objects SET sealed = (to_regclass('public.' || quote_ident(partition_name)) IS NULL);
CREATE INDEX idx_archive_objects_sealed ON audit_archive_objects (tenant_id, max_occurred_at) WHERE sealed;

-- ── apply_redaction: seal-aware `missed` + archive invalidation ─────────
CREATE OR REPLACE FUNCTION apply_redaction(p_task_id uuid)
RETURNS TABLE (task_status text, task_rows_redacted bigint)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = public AS $$
DECLARE
    t        audit_redaction_tasks%ROWTYPE;
    n        bigint;
    parts    text[];
    archived boolean;
    outcome  audit_redaction_status;
BEGIN
    SELECT * INTO t FROM audit_redaction_tasks WHERE id = p_task_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'apply_redaction: task % not found', p_task_id USING ERRCODE = 'no_data_found';
    END IF;
    IF t.status <> 'pending' THEN
        RETURN QUERY SELECT t.status::text, t.rows_redacted;
        RETURN;
    END IF;

    WITH upd AS (
        UPDATE audit_events e
           SET metadata = redaction_marker(t.id),
               actor_display = CASE WHEN e.actor_id = t.subject_actor_id THEN NULL ELSE e.actor_display END
         WHERE e.tenant_id = t.tenant_id
           AND e.retention_tier = 'security_3y'
           AND (e.actor_id = t.subject_actor_id
                OR (e.target_type = 'user' AND e.target_id = t.subject_actor_id::text))
           AND NOT (e.metadata ? '_redacted')
        RETURNING e.tableoid)
    SELECT count(*), array_agg(DISTINCT tableoid::regclass::text) INTO n, parts FROM upd;
    PERFORM invalidate_security_archive(parts);

    SELECT EXISTS (
        SELECT 1 FROM audit_archive_objects o
         WHERE o.tenant_id = t.tenant_id
           AND o.retention_tier = 'security_3y'
           AND o.sealed
           AND o.subject_ids @> ARRAY[t.subject_actor_id]
    ) INTO archived;

    outcome := CASE WHEN archived THEN 'missed'::audit_redaction_status
                    WHEN n > 0    THEN 'applied'::audit_redaction_status
                    ELSE               'not_applicable'::audit_redaction_status END;
    UPDATE audit_redaction_tasks
       SET status = outcome, applied_at = now(), rows_redacted = n, error = NULL
     WHERE id = t.id;
    INSERT INTO redacted_subjects (tenant_id, subject_id, task_id, redaction_completed_at)
    VALUES (t.tenant_id, t.subject_actor_id, t.id, now())
    ON CONFLICT (tenant_id, subject_id)
        DO UPDATE SET task_id = EXCLUDED.task_id, redaction_completed_at = EXCLUDED.redaction_completed_at;
    RETURN QUERY SELECT outcome::text, n;
END;
$$;

-- invalidate_security_archive(partitions): a rewritten security_3y row makes
-- that partition's uploaded (unsealed) security_3y objects stale; resetting
-- the tier to pending forces a re-archive before any drop can seal it.
CREATE OR REPLACE FUNCTION invalidate_security_archive(p_partitions text[])
RETURNS void LANGUAGE sql
    SET search_path = public AS $$
    UPDATE audit_event_archive_state
       SET status = 'pending', error = NULL
     WHERE retention_tier = 'security_3y'
       AND status IN ('archiving', 'archived', 'verified', 'failed')
       AND partition_name = ANY(coalesce(p_partitions, '{}'))
$$;
REVOKE ALL ON FUNCTION invalidate_security_archive(text[]) FROM PUBLIC;

-- ── sweep_redactions: + archive invalidation ────────────────────────────
CREATE OR REPLACE FUNCTION sweep_redactions(p_window interval)
RETURNS bigint
    LANGUAGE plpgsql
    SET search_path = public AS $$
DECLARE
    n     bigint;
    parts text[];
BEGIN
    IF p_window IS NULL OR p_window < interval '1 day' OR p_window > interval '400 days' THEN
        RAISE EXCEPTION 'sweep_redactions: window must be between 1 day and 400 days'
            USING ERRCODE = 'invalid_parameter_value';
    END IF;
    WITH upd AS (
        UPDATE audit_events e
           SET metadata = redaction_marker(r.task_id),
               actor_display = CASE WHEN e.actor_id = r.subject_id THEN NULL ELSE e.actor_display END
          FROM redacted_subjects r
         WHERE r.redaction_completed_at > now() - p_window
           AND e.tenant_id = r.tenant_id
           AND e.retention_tier = 'security_3y'
           AND (e.actor_id = r.subject_id
                OR (e.target_type = 'user' AND e.target_id = r.subject_id::text))
           AND NOT (e.metadata ? '_redacted')
        RETURNING e.tableoid)
    SELECT count(*), array_agg(DISTINCT tableoid::regclass::text) INTO n, parts FROM upd;
    PERFORM invalidate_security_archive(parts);
    RETURN n;
END;
$$;

-- ── audit_drop_partition: the AL-INV-9 gate ─────────────────────────────
CREATE OR REPLACE FUNCTION audit_drop_partition(p_partition text)
RETURNS text
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = public AS $$
DECLARE
    v_tier     audit_retention_tier;
    v_live     bigint;
    v_archived bigint;
    v_pending  boolean;
BEGIN
    IF p_partition IS NULL OR p_partition !~ '^audit_events_[0-9]{4}_[0-9]{2}$' THEN
        RAISE EXCEPTION 'audit_drop_partition: % is not a monthly partition name', p_partition
            USING ERRCODE = 'invalid_parameter_value';
    END IF;
    -- Parent first: the same lock order as an INSERT, so no deadlock; it
    -- also freezes the counts below until COMMIT.
    LOCK TABLE audit_events IN ACCESS EXCLUSIVE MODE;
    IF to_regclass('public.' || p_partition) IS NULL THEN
        RETURN 'missing';
    END IF;

    -- Every retained tier must be verified before any count is compared,
    -- so the reason reported is exact.
    FOREACH v_tier IN ARRAY ARRAY['security_3y', 'compliance_7y']::audit_retention_tier[] LOOP
        IF NOT EXISTS (SELECT 1 FROM audit_event_archive_state s
                        WHERE s.partition_name = p_partition AND s.retention_tier = v_tier AND s.status = 'verified') THEN
            RETURN 'not_verified';
        END IF;
    END LOOP;
    FOREACH v_tier IN ARRAY ARRAY['security_3y', 'compliance_7y']::audit_retention_tier[] LOOP
        EXECUTE format('SELECT count(*) FROM %I WHERE retention_tier = $1', p_partition) INTO v_live USING v_tier;
        SELECT coalesce(sum(o.row_count), 0) INTO v_archived FROM audit_archive_objects o
         WHERE o.partition_name = p_partition AND o.retention_tier = v_tier AND NOT o.sealed;
        IF v_live <> v_archived THEN
            RETURN 'count_mismatch';                       -- a late arrival since archival (AL-D4)
        END IF;
    END LOOP;

    EXECUTE format(
        'SELECT EXISTS (SELECT 1 FROM %I e JOIN audit_redaction_tasks r
                          ON r.tenant_id = e.tenant_id AND r.status = ''pending''
                         AND (e.actor_id = r.subject_actor_id
                              OR (e.target_type = ''user'' AND e.target_id = r.subject_actor_id::text))
                       WHERE e.retention_tier = ''security_3y'')', p_partition) INTO v_pending;
    IF v_pending THEN
        RETURN 'redaction_pending';                       -- AL-INV-12 backstop
    END IF;

    UPDATE audit_archive_objects SET sealed = true WHERE partition_name = p_partition AND NOT sealed;
    UPDATE audit_event_archive_state SET status = 'dropped', dropped_at = now()
     WHERE partition_name = p_partition AND retention_tier IN ('security_3y', 'compliance_7y');
    UPDATE audit_event_archive_state SET status = 'expired', dropped_at = now()
     WHERE partition_name = p_partition AND retention_tier = 'access_90d';
    EXECUTE format('ALTER TABLE audit_events DETACH PARTITION %I', p_partition);
    EXECUTE format('DROP TABLE %I', p_partition);
    RETURN 'dropped';
END;
$$;
REVOKE ALL ON FUNCTION audit_drop_partition(text) FROM PUBLIC;

-- ── audit_reopen_partition: D-19 automatic re-open of a dropped month ───
CREATE OR REPLACE FUNCTION audit_reopen_partition(p_partition text)
RETURNS bigint
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = public AS $$
DECLARE
    v_month date;
    v_from  timestamptz;
    v_to    timestamptz;
    v_moved bigint;
BEGIN
    IF p_partition IS NULL OR p_partition !~ '^audit_events_[0-9]{4}_[0-9]{2}$' THEN
        RAISE EXCEPTION 'audit_reopen_partition: % is not a monthly partition name', p_partition
            USING ERRCODE = 'invalid_parameter_value';
    END IF;
    v_month := to_date(substr(p_partition, 14), 'YYYY_MM');
    v_from  := v_month::timestamp AT TIME ZONE 'UTC';
    v_to    := (v_month + interval '1 month')::timestamp AT TIME ZONE 'UTC';

    LOCK TABLE audit_events IN ACCESS EXCLUSIVE MODE;
    IF to_regclass('public.' || p_partition) IS NOT NULL THEN
        RETURN 0;                                           -- already open
    END IF;
    IF NOT EXISTS (SELECT 1 FROM audit_event_archive_state
                    WHERE partition_name = p_partition AND status = 'dropped') THEN
        RAISE EXCEPTION 'audit_reopen_partition: % was never dropped — re-home by hand (RB-3)', p_partition
            USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    SELECT count(*) INTO v_moved FROM audit_events_default WHERE occurred_at >= v_from AND occurred_at < v_to;
    IF v_moved = 0 THEN
        RETURN 0;
    END IF;

    -- A month cannot be attached while DEFAULT holds rows in its range, so
    -- DEFAULT is swapped out and its rows re-routed through the parent.
    -- No row is ever UPDATEd or DELETEd (the append-only trigger is not
    -- involved); the old DEFAULT table is dropped whole once empty of
    -- meaning.
    ALTER TABLE audit_events DETACH PARTITION audit_events_default;
    ALTER TABLE audit_events_default RENAME TO audit_events_default_reopening;
    EXECUTE format('CREATE TABLE %I PARTITION OF audit_events FOR VALUES FROM (%L) TO (%L)',
                   p_partition,
                   to_char(v_from AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS') || '+00',
                   to_char(v_to   AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS') || '+00');
    CREATE TABLE audit_events_default PARTITION OF audit_events DEFAULT;
    INSERT INTO audit_events SELECT * FROM audit_events_default_reopening;
    DROP TABLE audit_events_default_reopening;

    UPDATE audit_event_archive_state
       SET status = 'pending', archived_at = NULL, verified_at = NULL, dropped_at = NULL, error = NULL
     WHERE partition_name = p_partition AND retention_tier IN ('security_3y', 'compliance_7y');
    RETURN v_moved;
END;
$$;
REVOKE ALL ON FUNCTION audit_reopen_partition(text) FROM PUBLIC;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'audit_reconciler') THEN
        GRANT UPDATE, DELETE ON audit_archive_objects TO audit_reconciler;   -- re-archive upsert / stale unsealed parts
        GRANT EXECUTE ON FUNCTION audit_drop_partition(text) TO audit_reconciler;
        GRANT EXECUTE ON FUNCTION audit_reopen_partition(text) TO audit_reconciler;
        GRANT EXECUTE ON FUNCTION invalidate_security_archive(text[]) TO audit_reconciler;
    END IF;
END$$;
