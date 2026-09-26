-- 000004_triggers — LLD §4.5.
--
-- Defense-in-depth behind the grant model (AL-INV-1): reject any UPDATE or
-- DELETE on audit_events from every role except audit_reconciler — which
-- also covers an accidental privileged session (the owner, a superuser).
-- Row triggers on the partitioned parent are cloned onto every partition,
-- including ones PartitionService creates later.
CREATE OR REPLACE FUNCTION forbid_audit_mutation() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF current_user <> 'audit_reconciler' THEN
        RAISE EXCEPTION 'audit_events is append-only (AL-INV-1): % denied for %', TG_OP, current_user
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN CASE TG_OP WHEN 'DELETE' THEN OLD ELSE NEW END;
END;
$$;

CREATE TRIGGER audit_events_no_update BEFORE UPDATE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION forbid_audit_mutation();
CREATE TRIGGER audit_events_no_delete BEFORE DELETE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION forbid_audit_mutation();

-- audit_export_jobs is a mutable job-status table (not audit content):
-- conventional updated_at touch, fired only when the row actually changed.
CREATE OR REPLACE FUNCTION touch_row() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$;

CREATE TRIGGER audit_export_jobs_touch BEFORE UPDATE ON audit_export_jobs
    FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*) EXECUTE FUNCTION touch_row();
