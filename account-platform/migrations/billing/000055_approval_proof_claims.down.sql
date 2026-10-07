DO $$
BEGIN
    RAISE EXCEPTION 'Billing approval proof claims are irreversible; retain migration 000055' USING ERRCODE='55000';
END $$;
