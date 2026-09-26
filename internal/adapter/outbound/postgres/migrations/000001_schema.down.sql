-- Reverses 000001_schema.
DROP TABLE IF EXISTS audit_redaction_tasks;
DROP TABLE IF EXISTS audit_export_jobs;
DROP TABLE IF EXISTS tenant_plan_window;
DROP TABLE IF EXISTS processed_events;
DROP TABLE IF EXISTS audit_event_archive_state;
DROP TABLE IF EXISTS audit_events;   -- drops every partition, incl. audit_events_default

DROP TYPE IF EXISTS audit_redaction_status;
DROP TYPE IF EXISTS audit_export_status;
DROP TYPE IF EXISTS audit_archive_status;
DROP TYPE IF EXISTS audit_ingest_mode;
DROP TYPE IF EXISTS audit_actor_type;
DROP TYPE IF EXISTS audit_retention_tier;
-- pgcrypto is left installed: other databases/objects may rely on it and
-- dropping an extension is not an additive-safe down step.
