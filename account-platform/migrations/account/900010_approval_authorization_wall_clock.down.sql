DO $$
BEGIN
  IF session_user<>'account_migrator' OR current_user<>'account_service' THEN
    RAISE EXCEPTION '900010 down requires isolated account migration identities' USING ERRCODE='42501';
  END IF;
  RAISE EXCEPTION 'ACCOUNT_APPROVAL_AUTHORIZATION_WALL_CLOCK_FORWARD_ONLY: preserve current authorization fences' USING ERRCODE='55000';
END $$;
