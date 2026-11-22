DO $$
BEGIN
  IF session_user<>'account_migrator' OR current_user<>'account_service' THEN
    RAISE EXCEPTION '000065 down requires the isolated account migration identities' USING ERRCODE='42501';
  END IF;
  RAISE EXCEPTION 'ACCOUNT_APPROVAL_FAILURE_FORWARD_ONLY: retain proof-era query-only failure state across application rollback; use a reviewed forward correction' USING ERRCODE='55000';
END $$;
