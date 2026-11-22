DO $$
DECLARE
    accepting BOOLEAN := FALSE;
    active_count BIGINT := 0;
BEGIN
    IF to_regclass('token_admin_pagination_control') IS NOT NULL THEN
        SELECT accepting_new_snapshots INTO accepting
        FROM token_admin_pagination_control
        WHERE singleton=TRUE;
        IF COALESCE(accepting,FALSE) THEN
            RAISE EXCEPTION 'TOKEN_ADMIN_PAGINATION_GATE_ENABLED: stop new snapshots before down migration';
        END IF;
    END IF;
    IF to_regclass('token_admin_pagination_snapshots') IS NOT NULL THEN
        SELECT count(*) INTO active_count
        FROM token_admin_pagination_snapshots
        WHERE expires_at>transaction_timestamp();
        IF active_count>0 THEN
            RAISE EXCEPTION 'TOKEN_ADMIN_PAGINATION_CURSOR_ACTIVE: wait for % unexpired snapshots before down migration', active_count;
        END IF;
    END IF;
END $$;

DROP TABLE IF EXISTS token_admin_pagination_creation_windows;
DROP TABLE IF EXISTS token_admin_pagination_snapshot_tombstones;
DROP TABLE IF EXISTS token_admin_pagination_snapshot_members;
DROP TABLE IF EXISTS token_admin_pagination_snapshots;
DROP TABLE IF EXISTS token_admin_pagination_control;
