DO $$
DECLARE accepting BOOLEAN; active_count BIGINT; tombstone_count BIGINT; signing_key_count BIGINT;
BEGIN
  IF session_user<>'account_migrator' OR current_user<>'account_service' THEN
    RAISE EXCEPTION '000061 down requires the isolated account migration identities' USING ERRCODE='42501';
  END IF;
  SELECT accepting_new_snapshots INTO accepting FROM approval_pagination_control WHERE singleton=TRUE;
  IF COALESCE(accepting,FALSE) THEN
    RAISE EXCEPTION 'ACCOUNT_APPROVAL_PAGINATION_GATE_ENABLED: stop new snapshots before down migration' USING ERRCODE='55000';
  END IF;
  SELECT count(*) INTO active_count FROM approval_pagination_snapshots WHERE expires_at>transaction_timestamp();
  IF active_count>0 THEN
    RAISE EXCEPTION 'ACCOUNT_APPROVAL_PAGINATION_CURSOR_ACTIVE: wait for % unexpired snapshots before down migration',active_count USING ERRCODE='55000';
  END IF;
  SELECT count(*) INTO tombstone_count FROM approval_pagination_snapshot_tombstones WHERE purge_after>transaction_timestamp();
  IF tombstone_count>0 THEN
    RAISE EXCEPTION 'ACCOUNT_APPROVAL_PAGINATION_TOMBSTONE_ACTIVE: wait for % retained tombstones before down migration',tombstone_count USING ERRCODE='55000';
  END IF;
  SELECT count(*) INTO signing_key_count FROM approval_pagination_signing_keys WHERE last_signed_at+make_interval(secs=>2100)>transaction_timestamp();
  IF signing_key_count>0 THEN
    RAISE EXCEPTION 'ACCOUNT_APPROVAL_PAGINATION_KEY_ACTIVE: wait for % retained signing keys before down migration',signing_key_count USING ERRCODE='55000';
  END IF;
END $$;

DROP TABLE approval_pagination_rate_windows;
DROP TABLE approval_pagination_signing_keys;
DROP TABLE approval_pagination_snapshot_tombstones;
DROP TABLE approval_pagination_snapshot_members;
DROP TABLE approval_pagination_snapshots;
DROP TABLE approval_pagination_control;
