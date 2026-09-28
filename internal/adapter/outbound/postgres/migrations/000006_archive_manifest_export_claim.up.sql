-- 000006_archive_manifest_export_claim — Phase 4 (decisions D-2, D-10, D-12).
--
-- audit_archive_objects: one row per archived S3 object. Archive objects are
-- partitioned by tenant and month (D-10), so each row is tenant-scoped and
-- RLS-protected like audit_events. Written by the reconciler's archival step
-- (Phase 7); read by the query path to (a) find a tenant's objects for a
-- month whose hot partition was dropped (D-12) and (b) estimate an archived
-- read's size against ARCHIVE_SYNC_MAX_ROWS / ARCHIVE_SYNC_MAX_BYTES (D-10)
-- and (c) find the few objects that can hold an archived AL-2 id.
CREATE TABLE audit_archive_objects (
    partition_name  text NOT NULL,                       -- audit_events_YYYY_MM
    retention_tier  audit_retention_tier NOT NULL,
    tenant_id       uuid NOT NULL,                       -- RLS scope
    part            int  NOT NULL,                       -- NNNN in the key
    period_month    date NOT NULL,                       -- first day of the month
    s3_bucket       text NOT NULL,
    s3_key          text NOT NULL,                       -- {tier}/{tenant}/{yyyy}/{mm}/audit_events_{yyyy}_{mm}-part-NNNN.jsonl.gz
    row_count       bigint NOT NULL,
    byte_size       bigint NOT NULL,                     -- uncompressed JSONL bytes (estimate basis)
    min_occurred_at timestamptz NOT NULL,
    max_occurred_at timestamptz NOT NULL,
    min_id          uuid NOT NULL,                       -- id range of the object's rows (UUIDv7):
    max_id          uuid NOT NULL,                       --   AL-2 reads only objects whose range holds the id
    sha256          text NOT NULL,                       -- checksum of the object body (verification, §15.4)
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT audit_archive_objects_pkey PRIMARY KEY (partition_name, retention_tier, tenant_id, part),
    CONSTRAINT uq_archive_objects_key UNIQUE (s3_bucket, s3_key),
    CONSTRAINT chk_archive_objects_counts CHECK (row_count >= 0 AND byte_size >= 0 AND part >= 0),
    CONSTRAINT chk_archive_objects_ranges CHECK (min_occurred_at <= max_occurred_at AND min_id <= max_id)
);
CREATE INDEX idx_archive_objects_tenant_month ON audit_archive_objects (tenant_id, period_month);
CREATE INDEX idx_archive_objects_tenant_ids   ON audit_archive_objects (tenant_id, min_id, max_id);

ALTER TABLE audit_archive_objects ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_archive_objects FORCE  ROW LEVEL SECURITY;
REVOKE ALL ON audit_archive_objects FROM PUBLIC;
CREATE POLICY tenant_isolation ON audit_archive_objects
    USING      (rls_check_tenant(tenant_id, 'audit_archive_objects'))
    WITH CHECK (rls_check_tenant(tenant_id, 'audit_archive_objects'));

-- claim_export_job() — decision D-2. The export worker runs in cmd/server as
-- audit_app, which under RLS sees only one tenant at a time; this narrow
-- SECURITY DEFINER function claims the oldest pending job across tenants
-- (or a 'running' one whose lease expired — a crashed worker), marks it
-- running, and returns just enough to process it under that tenant's own
-- GUC. Owned by the (BYPASSRLS) migrating role.
CREATE OR REPLACE FUNCTION claim_export_job(p_lease interval)
RETURNS TABLE (job_id uuid, job_tenant_id uuid, job_requested_by uuid, job_filter jsonb)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = public AS $$
BEGIN
    IF p_lease IS NULL OR p_lease < interval '1 minute' OR p_lease > interval '1 day' THEN
        RAISE EXCEPTION 'claim_export_job: lease must be between 1 minute and 1 day'
            USING ERRCODE = 'invalid_parameter_value';
    END IF;
    RETURN QUERY
    UPDATE audit_export_jobs j
       SET status = 'running', error = NULL, updated_at = now()  -- explicit: touch_row skips no-op updates (lease re-claim)
     WHERE j.id = (
            SELECT c.id FROM audit_export_jobs c
             WHERE c.status = 'pending'
                OR (c.status = 'running' AND c.updated_at < now() - p_lease)
             ORDER BY c.created_at
             FOR UPDATE SKIP LOCKED
             LIMIT 1)
    RETURNING j.id, j.tenant_id, j.requested_by, j.filter;
END;
$$;
REVOKE ALL ON FUNCTION claim_export_job(interval) FROM PUBLIC;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'audit_app') THEN
        GRANT SELECT ON audit_archive_objects TO audit_app;
        GRANT EXECUTE ON FUNCTION claim_export_job(interval) TO audit_app;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'audit_reconciler') THEN
        GRANT SELECT, INSERT ON audit_archive_objects TO audit_reconciler;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'admin_readonly') THEN
        GRANT SELECT ON audit_archive_objects TO admin_readonly;
    END IF;
END$$;
