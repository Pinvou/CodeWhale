DO $$
BEGIN
    IF session_user<>'account_migrator'
       AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname=session_user AND rolsuper) THEN
        RAISE EXCEPTION '000050 down requires the dedicated account_migrator identity';
    END IF;
    IF pg_catalog.to_regclass('account_approval_control.admin_approval_control') IS NOT NULL
       AND EXISTS (SELECT 1 FROM account_approval_control.admin_approval_control) THEN
        RAISE EXCEPTION 'ADMIN_APPROVAL control history must be preserved' USING ERRCODE='55000';
    END IF;
END $$;

SET ROLE account_approval_control_owner;
DROP FUNCTION IF EXISTS account_approval_control.lock_admin_approval_control_shared();
DROP TABLE IF EXISTS account_approval_control.admin_approval_control;
DROP FUNCTION IF EXISTS account_approval_control.enforce_admin_approval_control_update();
DROP FUNCTION IF EXISTS account_approval_control.reject_admin_approval_control_destructive();
DROP SCHEMA IF EXISTS account_approval_control;
SET ROLE account_service;

