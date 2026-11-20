DO $$ BEGIN
    IF session_user <> 'account_migrator' OR current_user <> 'account_service' THEN
        RAISE EXCEPTION 'commerce manual completion requires the Account migrator' USING ERRCODE='42501';
    END IF;
    RAISE EXCEPTION 'commerce manual completion is forward-only' USING ERRCODE='55000';
END $$;
