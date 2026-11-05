DO $$
BEGIN
  IF session_user<>'account_migrator' OR current_user<>'account_service' THEN
    RAISE EXCEPTION '000067 down requires the isolated account migration identities' USING ERRCODE='42501';
  END IF;
  RAISE EXCEPTION 'ACCOUNT_APPROVAL_SINGLE_ATTEMPT_FENCE_FORWARD_ONLY: preserve proof-era query-only attempts across application rollback' USING ERRCODE='55000';
END $$;
