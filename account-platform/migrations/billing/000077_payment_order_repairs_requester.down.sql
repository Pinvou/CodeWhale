DO $$
BEGIN
    RAISE EXCEPTION 'payment financial repair requester column is forward-only; retain migration 000077' USING ERRCODE='55000';
END $$;
