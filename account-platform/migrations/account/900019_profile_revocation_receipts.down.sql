DO $$ BEGIN
    RAISE EXCEPTION 'profile session revocation receipts are forward-only' USING ERRCODE='55000';
END $$;
