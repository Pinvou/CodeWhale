DO $$
BEGIN
  IF session_user<>'account_migrator' OR current_user<>'account_service' THEN
    RAISE EXCEPTION '900009 down requires the isolated account migration identities' USING ERRCODE='42501';
  END IF;
  RAISE EXCEPTION 'ACCOUNT_APPROVAL_RECOVERY_QUERY_MATRIX_FORWARD_ONLY: preserve bounded recovery for registered original executions' USING ERRCODE='55000';
END $$;
