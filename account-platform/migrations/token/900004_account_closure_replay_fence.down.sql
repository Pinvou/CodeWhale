DO $$
BEGIN
    RAISE EXCEPTION 'anonymized user replay fences are forward-only'
        USING ERRCODE = '55000';
END;
$$;
