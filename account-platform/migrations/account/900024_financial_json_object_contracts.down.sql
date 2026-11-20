DO $$ BEGIN
 IF session_user<>'account_migrator' OR current_user<>'account_service' THEN
  RAISE EXCEPTION '900024 requires isolated account migration identities' USING ERRCODE='42501';
 END IF;
 RAISE EXCEPTION 'financial JSON object contracts are forward-only' USING ERRCODE='55000';
END $$;
