DO $$
DECLARE is_superuser BOOLEAN;
BEGIN
    SELECT rolsuper INTO is_superuser FROM pg_catalog.pg_roles WHERE rolname=session_user;
    IF session_user<>'billing_migrator' AND NOT is_superuser THEN
        RAISE EXCEPTION '000053 requires the dedicated billing_migrator deployment identity';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname='billing_service')
       OR NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname='billing_wallet_receipt_deployer')
       OR NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname='billing_schema_owner' AND NOT rolcanlogin)
       OR NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname='billing_migrator' AND rolcanlogin AND NOT rolinherit) THEN
        RAISE EXCEPTION '000053 deployment roles must be provisioned before migration';
    END IF;
    IF session_user='billing_migrator' AND (NOT pg_catalog.pg_has_role(session_user,'billing_service','MEMBER') OR NOT pg_catalog.pg_has_role(session_user,'billing_wallet_receipt_deployer','MEMBER')) THEN
        RAISE EXCEPTION 'billing_migrator role membership is incomplete';
    END IF;
    IF NOT is_superuser AND current_user<>'billing_service' THEN
        RAISE EXCEPTION '000053 must run with MIGRATION_ROLE=billing_service';
    END IF;
    IF session_user='billing_migrator' AND NOT pg_catalog.pg_has_role(session_user,'billing_schema_owner','MEMBER') THEN
        RAISE EXCEPTION 'billing_migrator must be a member of billing_schema_owner';
    END IF;
    IF pg_catalog.pg_has_role('billing_service','billing_schema_owner','MEMBER') THEN
        RAISE EXCEPTION 'runtime must not be a member of billing_schema_owner';
    END IF;
    IF pg_catalog.pg_has_role('billing_service',(SELECT datdba FROM pg_catalog.pg_database WHERE datname=current_database()),'MEMBER') THEN
        RAISE EXCEPTION 'billing runtime must not own or inherit the database owner';
    END IF;
    IF (SELECT pg_catalog.pg_get_userbyid(nspowner) FROM pg_catalog.pg_namespace WHERE oid=pg_catalog.to_regnamespace('public')) IS DISTINCT FROM 'billing_schema_owner' THEN
        RAISE EXCEPTION 'public schema must be owned by billing_schema_owner';
    END IF;
    EXECUTE 'GRANT REFERENCES ON public.wallets TO billing_wallet_receipt_deployer';
END $$;
SET ROLE billing_schema_owner;
GRANT USAGE,CREATE ON SCHEMA public TO billing_wallet_receipt_deployer;
REVOKE CREATE ON SCHEMA public FROM billing_service,PUBLIC;
SET ROLE billing_wallet_receipt_deployer;
CREATE TABLE public.token_account_wallet_provisioning_receipts (
    command_reference UUID PRIMARY KEY,
    owner_type TEXT NOT NULL CHECK(owner_type IN ('PERSONAL','ORGANIZATION','ORG_UNIT')),
    owner_id UUID NOT NULL,
    request_digest CHAR(64) NOT NULL CHECK(request_digest ~ '^[0-9a-f]{64}$'),
    status TEXT NOT NULL CHECK(status IN ('PENDING','SUCCEEDED','FAILED','UNKNOWN')),
    wallet_id UUID REFERENCES public.wallets(id),
    stable_error_code TEXT,
    attempt_count INTEGER NOT NULL DEFAULT 1 CHECK(attempt_count>0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    completed_at TIMESTAMPTZ,
    CHECK(
      (status='PENDING' AND wallet_id IS NULL AND stable_error_code IS NULL AND completed_at IS NULL) OR
      (status='SUCCEEDED' AND wallet_id IS NOT NULL AND stable_error_code IS NULL AND completed_at IS NOT NULL) OR
      (status IN ('FAILED','UNKNOWN') AND wallet_id IS NULL AND stable_error_code IS NOT NULL AND completed_at IS NOT NULL)
    ),
    CHECK(stable_error_code IS NULL OR (stable_error_code ~ '^[A-Z][A-Z0-9_]{0,119}$' AND stable_error_code !~ '(TOKEN|KEY|SECRET|PASSWORD|CREDENTIAL)'))
);
CREATE FUNCTION public.reject_token_account_wallet_receipt_mutation() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN RAISE EXCEPTION 'wallet provisioning receipt is immutable' USING ERRCODE='55000'; END $$;
CREATE TRIGGER token_account_wallet_receipt_immutable BEFORE UPDATE OR DELETE ON public.token_account_wallet_provisioning_receipts
FOR EACH ROW EXECUTE FUNCTION public.reject_token_account_wallet_receipt_mutation();
REVOKE ALL ON public.token_account_wallet_provisioning_receipts FROM PUBLIC;
GRANT SELECT,INSERT ON public.token_account_wallet_provisioning_receipts TO billing_service;
SET ROLE billing_schema_owner;
REVOKE CREATE ON SCHEMA public FROM billing_wallet_receipt_deployer;
SET ROLE billing_service;
