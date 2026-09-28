-- Reverses 000008_reconciler: restores the 000007 apply_redaction() and
-- sweep_redactions() (partition-existence routing, no archive invalidation).
DROP FUNCTION IF EXISTS audit_reopen_partition(text);
DROP FUNCTION IF EXISTS audit_drop_partition(text);
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'audit_reconciler') THEN
        REVOKE UPDATE, DELETE ON audit_archive_objects FROM audit_reconciler;
    END IF;
END$$;

CREATE OR REPLACE FUNCTION apply_redaction(p_task_id uuid)
RETURNS TABLE (task_status text, task_rows_redacted bigint)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = public AS $$
DECLARE
    t        audit_redaction_tasks%ROWTYPE;
    n        bigint;
    archived boolean;
    outcome  audit_redaction_status;
BEGIN
    SELECT * INTO t FROM audit_redaction_tasks WHERE id = p_task_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'apply_redaction: task % not found', p_task_id USING ERRCODE = 'no_data_found';
    END IF;
    IF t.status <> 'pending' THEN          -- idempotent: a finished task is reported, never re-run
        RETURN QUERY SELECT t.status::text, t.rows_redacted;
        RETURN;
    END IF;

    -- D-15: security_3y only (compliance_7y is never touched, AL-INV-7).
    -- Rows where the subject is the actor or the user target: metadata is
    -- replaced whole by a marker, and actor_display is cleared on the
    -- subject's own rows. actor_id/target_id (opaque subs) are kept (§15.5).
    UPDATE audit_events e
       SET metadata = redaction_marker(t.id),
           actor_display = CASE WHEN e.actor_id = t.subject_actor_id THEN NULL ELSE e.actor_display END
     WHERE e.tenant_id = t.tenant_id
       AND e.retention_tier = 'security_3y'
       AND (e.actor_id = t.subject_actor_id
            OR (e.target_type = 'user' AND e.target_id = t.subject_actor_id::text))
       AND NOT (e.metadata ? '_redacted');
    GET DIAGNOSTICS n = ROW_COUNT;

    -- D-17: the subject has rows in an archived (partition-dropped, D-12)
    -- security_3y object, which are under Object Lock and unreachable → missed.
    SELECT EXISTS (
        SELECT 1 FROM audit_archive_objects o
         WHERE o.tenant_id = t.tenant_id
           AND o.retention_tier = 'security_3y'
           AND o.subject_ids @> ARRAY[t.subject_actor_id]
           AND to_regclass('public.' || quote_ident(o.partition_name)) IS NULL
    ) INTO archived;

    outcome := CASE WHEN archived THEN 'missed'::audit_redaction_status
                    WHEN n > 0    THEN 'applied'::audit_redaction_status
                    ELSE               'not_applicable'::audit_redaction_status END;
    UPDATE audit_redaction_tasks
       SET status = outcome, applied_at = now(), rows_redacted = n, error = NULL
     WHERE id = t.id;
    -- D-18: every finished task marks the subject erased, so a row about
    -- them that arrives later is redacted on ingest (gap 37).
    INSERT INTO redacted_subjects (tenant_id, subject_id, task_id, redaction_completed_at)
    VALUES (t.tenant_id, t.subject_actor_id, t.id, now())
    ON CONFLICT (tenant_id, subject_id)
        DO UPDATE SET task_id = EXCLUDED.task_id, redaction_completed_at = EXCLUDED.redaction_completed_at;
    RETURN QUERY SELECT outcome::text, n;
END;
$$;

CREATE OR REPLACE FUNCTION sweep_redactions(p_window interval)
RETURNS bigint
    LANGUAGE plpgsql
    SET search_path = public AS $$
DECLARE
    n bigint;
BEGIN
    IF p_window IS NULL OR p_window < interval '1 day' OR p_window > interval '400 days' THEN
        RAISE EXCEPTION 'sweep_redactions: window must be between 1 day and 400 days'
            USING ERRCODE = 'invalid_parameter_value';
    END IF;
    UPDATE audit_events e
       SET metadata = redaction_marker(r.task_id),
           actor_display = CASE WHEN e.actor_id = r.subject_id THEN NULL ELSE e.actor_display END
      FROM redacted_subjects r
     WHERE r.redaction_completed_at > now() - p_window
       AND e.tenant_id = r.tenant_id
       AND e.retention_tier = 'security_3y'
       AND (e.actor_id = r.subject_id
            OR (e.target_type = 'user' AND e.target_id = r.subject_id::text))
       AND NOT (e.metadata ? '_redacted');
    GET DIAGNOSTICS n = ROW_COUNT;
    RETURN n;
END;
$$;

DROP FUNCTION IF EXISTS invalidate_security_archive(text[]);
DROP INDEX IF EXISTS idx_archive_objects_sealed;
ALTER TABLE audit_archive_objects DROP COLUMN IF EXISTS sealed;
