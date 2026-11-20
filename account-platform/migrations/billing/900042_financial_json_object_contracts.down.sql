DO $$ BEGIN
 IF session_user<>'billing_migrator' OR current_user<>'billing_service' THEN
  RAISE EXCEPTION '900042 requires isolated billing migration identities' USING ERRCODE='42501';
 END IF;
 RAISE EXCEPTION 'financial JSON object contracts are forward-only' USING ERRCODE='55000';
END $$;
