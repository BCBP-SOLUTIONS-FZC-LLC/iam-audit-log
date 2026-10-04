-- 000011_reconciler_lease — one run at a time per reconciler job.
--
-- concurrencyPolicy: Forbid stops a CronJob from overlapping its own
-- schedule, but not a run started by hand (kubectl create job --from=…, as
-- the runbooks instruct). Two concurrent `reconcile` runs would archive the
-- same tiers twice. A session advisory lock does not survive PgBouncer's
-- transaction mode, so the run holds a lease row instead: claimed when free
-- or expired, released at the end, and expiring on its own if the run dies.
CREATE TABLE reconciler_job_lease (
    job        text        PRIMARY KEY,
    holder     text        NOT NULL,
    expires_at timestamptz NOT NULL
);
REVOKE ALL ON reconciler_job_lease FROM PUBLIC;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'audit_reconciler') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON reconciler_job_lease TO audit_reconciler;
    END IF;
END$$;
