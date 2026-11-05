DO $$
BEGIN
  IF session_user<>'account_migrator' OR current_user<>'account_service' THEN
    RAISE EXCEPTION '000062 down requires the isolated account migration identities' USING ERRCODE='42501';
  END IF;
  IF EXISTS(SELECT 1 FROM approval_pagination_signing_keys
            WHERE last_signed_at+make_interval(secs=>2100)>transaction_timestamp()) THEN
    RAISE EXCEPTION 'ACCOUNT_APPROVAL_PAGINATION_KEY_ACTIVE: wait for retained signing keys before down migration' USING ERRCODE='55000';
  END IF;
END $$;

ALTER TABLE approval_pagination_signing_keys DROP COLUMN key_digest;
