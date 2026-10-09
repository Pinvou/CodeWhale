DO $$
BEGIN
    RAISE EXCEPTION 'account closure cleanup acknowledgements are forward-only'
        USING ERRCODE = '55000';
END;
$$;
