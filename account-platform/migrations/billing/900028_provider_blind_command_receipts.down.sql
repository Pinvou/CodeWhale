DO $$ BEGIN
    RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = 'provider blind command receipts are forward-only';
END $$;
