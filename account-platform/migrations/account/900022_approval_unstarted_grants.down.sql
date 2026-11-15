DO $$ BEGIN
  IF session_user<>'account_migrator' OR current_user<>'account_service' THEN
    RAISE EXCEPTION '900022 down requires isolated migration identities' USING ERRCODE='42501';
  END IF;
  RAISE EXCEPTION '900022 inventory is forward-only; preserve recovery visibility' USING ERRCODE='55000';
END $$;
