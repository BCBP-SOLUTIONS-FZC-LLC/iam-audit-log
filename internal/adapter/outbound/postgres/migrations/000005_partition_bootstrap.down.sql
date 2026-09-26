-- Reverses 000005_partition_bootstrap: drops the function and every EMPTY
-- explicit monthly partition. A partition holding rows is left attached —
-- a down migration must never destroy audit data (AL-INV-1); 000001's down
-- removes the whole table if the schema itself is being rolled back.
DO $$
DECLARE
    p     regclass;
    v_has boolean;
BEGIN
    FOR p IN
        SELECT c.oid::regclass
        FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
        WHERE i.inhparent = 'audit_events'::regclass AND c.relname ~ '^audit_events_[0-9]{4}_[0-9]{2}$'
    LOOP
        EXECUTE format('SELECT EXISTS (SELECT 1 FROM %s)', p) INTO v_has;
        IF NOT v_has THEN
            EXECUTE format('DROP TABLE %s', p);
        END IF;
    END LOOP;
END$$;

DROP FUNCTION IF EXISTS audit_ensure_partitions(int, int);
