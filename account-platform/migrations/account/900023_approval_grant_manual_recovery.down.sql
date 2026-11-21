DO $$ BEGIN
 IF session_user<>'account_migrator' OR current_user<>'account_service' THEN
  RAISE EXCEPTION '900023 requires isolated migration identities' USING ERRCODE='42501';
 END IF;
 RAISE EXCEPTION 'manual recovery evidence and execution fence are forward-only' USING ERRCODE='55000';
END $$;
