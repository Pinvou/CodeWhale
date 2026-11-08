DO $$
BEGIN
    RAISE EXCEPTION 'usage loss review settled state is forward-only; retain migration 000075' USING ERRCODE='55000';
END $$;
