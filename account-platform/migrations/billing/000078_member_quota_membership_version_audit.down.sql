SET ROLE billing_service;
DO $$
BEGIN
    RAISE EXCEPTION 'member quota membership-version reconciliation is forward-only; retain migration 000078' USING ERRCODE='55000';
END $$;
