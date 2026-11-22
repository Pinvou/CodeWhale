DO $$
BEGIN
  IF session_user<>'account_migrator' OR current_user<>'account_service' THEN
    RAISE EXCEPTION '000064 down requires the isolated account migration identities' USING ERRCODE='42501';
  END IF;
  RAISE EXCEPTION 'ACCOUNT_APPROVAL_PAGINATION_FORWARD_ONLY: retain schema during application rollback; correct database issues with a new forward migration' USING ERRCODE='55000';
END $$;
