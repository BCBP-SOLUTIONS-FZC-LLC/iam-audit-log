-- Reverses 000004_triggers.
DROP TRIGGER IF EXISTS audit_export_jobs_touch ON audit_export_jobs;
DROP FUNCTION IF EXISTS touch_row();
DROP TRIGGER IF EXISTS audit_events_no_delete ON audit_events;
DROP TRIGGER IF EXISTS audit_events_no_update ON audit_events;
DROP FUNCTION IF EXISTS forbid_audit_mutation();
