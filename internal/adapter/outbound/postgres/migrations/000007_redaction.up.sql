-- 000007_redaction — Phase 6 (LLD §8.7, §15.5; AL-INV-7, AL-INV-12;
-- decisions D-1, D-15, D-16, D-17).
--
-- 1. audit_archive_objects.subject_ids (D-17): the distinct actor ids and
--    user-target ids in an archived object, written by the Phase 7
--    archiver. It lets a redaction task check exactly and cheaply whether
--    any of the subject's rows are already archived (`missed`, AL-Q15)
--    without scanning S3.
-- 2. audit_reconciler gets a column-level UPDATE on audit_events: metadata
--    and actor_display only, the two columns redaction rewrites (D-15). The
--    audit_events trigger (000004) already rejects any other role.
-- 3. apply_redaction(task_id) (D-1): a SECURITY DEFINER function owned by
--    audit_reconciler, so the trigger's current_user check passes. cmd/server
--    (audit_app, EXECUTE only) calls it right after the UserDeleted ingest
--    commits; the reconciler's redaction-retry job calls it for stuck tasks.
-- 4. redacted_subjects (D-18, gap 37): one row per erased subject. Every
--    finished task records it here; the ingest path reads it (audit_app,
--    SELECT under RLS) and redacts a late-arriving security_3y row BEFORE
--    insert. sweep_redactions() is the daily defense-in-depth re-check.
--    redaction_marker() is the single definition of the marker; the Go
--    ingest path builds the same keys (domain.RedactedMetadata).

CREATE TABLE redacted_subjects (
    tenant_id              uuid NOT NULL,
    subject_id             uuid NOT NULL,
    task_id                uuid NOT NULL,                -- the task whose marker late rows carry
    redaction_completed_at timestamptz NOT NULL,
    CONSTRAINT redacted_subjects_pkey PRIMARY KEY (tenant_id, subject_id)
);
CREATE INDEX idx_redacted_subjects_completed ON redacted_subjects (redaction_completed_at);
ALTER TABLE redacted_subjects ENABLE ROW LEVEL SECURITY;
ALTER TABLE redacted_subjects FORCE  ROW LEVEL SECURITY;
REVOKE ALL ON redacted_subjects FROM PUBLIC;
CREATE POLICY tenant_isolation ON redacted_subjects
    USING      (rls_check_tenant(tenant_id, 'redacted_subjects'))
    WITH CHECK (rls_check_tenant(tenant_id, 'redacted_subjects'));

CREATE OR REPLACE FUNCTION redaction_marker(p_task_id uuid, p_on_ingest boolean DEFAULT false)
RETURNS jsonb LANGUAGE sql STABLE AS $$
    SELECT jsonb_build_object('_redacted', true, '_redaction_task_id', p_task_id, '_redacted_at', now())
           || CASE WHEN p_on_ingest THEN jsonb_build_object('_redacted_on_ingest', true) ELSE '{}'::jsonb END
$$;

ALTER TABLE audit_archive_objects ADD COLUMN subject_ids uuid[] NOT NULL DEFAULT '{}';
CREATE INDEX idx_archive_objects_subjects ON audit_archive_objects USING gin (subject_ids);

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
REVOKE ALL ON FUNCTION apply_redaction(uuid) FROM PUBLIC;

-- sweep_redactions(window) — D-18 defense in depth, run daily by the
-- reconciler (redaction-sweep job, as audit_reconciler). It re-redacts any
-- unredacted security_3y row of a subject whose redaction finished within
-- the window: a row that slipped past the ingest check (a race with a
-- concurrent apply, a bug, a manual backfill). Returns the rows fixed;
-- anything above zero means the ingest check was bypassed.
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
REVOKE ALL ON FUNCTION sweep_redactions(interval) FROM PUBLIC;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'audit_reconciler') THEN
        GRANT UPDATE (metadata, actor_display) ON audit_events TO audit_reconciler;
        -- D-1: the definer runs as audit_reconciler. The migrating role must
        -- be able to SET ROLE audit_reconciler (superuser in dev/test;
        -- Terraform grants the membership in AWS, BUILD_PLAN gap 34).
        ALTER FUNCTION apply_redaction(uuid) OWNER TO audit_reconciler;
        GRANT SELECT, INSERT, UPDATE ON redacted_subjects TO audit_reconciler;
        GRANT EXECUTE ON FUNCTION sweep_redactions(interval) TO audit_reconciler;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'audit_app') THEN
        GRANT EXECUTE ON FUNCTION apply_redaction(uuid) TO audit_app;
        GRANT SELECT ON redacted_subjects TO audit_app;              -- D-18 ingest-time check
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'admin_readonly') THEN
        GRANT SELECT ON redacted_subjects TO admin_readonly;
    END IF;
END$$;
