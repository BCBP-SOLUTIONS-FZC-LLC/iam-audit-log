-- Reverses 000007_redaction.
DROP FUNCTION IF EXISTS sweep_redactions(interval);
DROP FUNCTION IF EXISTS apply_redaction(uuid);
DROP FUNCTION IF EXISTS redaction_marker(uuid, boolean);
DROP TABLE IF EXISTS redacted_subjects;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'audit_reconciler') THEN
        REVOKE UPDATE (metadata, actor_display) ON audit_events FROM audit_reconciler;
    END IF;
END$$;
DROP INDEX IF EXISTS idx_archive_objects_subjects;
ALTER TABLE audit_archive_objects DROP COLUMN IF EXISTS subject_ids;
