SET ROLE billing_service;
DO $$
DECLARE is_superuser BOOLEAN;
BEGIN
    SELECT rolsuper INTO is_superuser FROM pg_catalog.pg_roles WHERE rolname=session_user;
    IF session_user<>'billing_migrator' AND NOT is_superuser THEN
        RAISE EXCEPTION '000055 requires the dedicated billing_migrator deployment identity';
    END IF;
    IF NOT is_superuser AND current_user<>'billing_service' THEN
        RAISE EXCEPTION '000055 must run with MIGRATION_ROLE=billing_service';
    END IF;
    IF NOT pg_catalog.pg_has_role(session_user,'billing_schema_owner','MEMBER')
       OR pg_catalog.pg_has_role('billing_service','billing_schema_owner','MEMBER') THEN
        RAISE EXCEPTION '000055 schema owner must remain separate from runtime';
    END IF;
END $$;

SET ROLE billing_schema_owner;
CREATE TABLE public.billing_approval_proof_claims (
    approval_request_id UUID PRIMARY KEY,
    authorization_event_id UUID NOT NULL UNIQUE,
    command_type TEXT NOT NULL CHECK(command_type IN ('WALLET_ADJUSTMENT','PAYMENT_PROVIDER_RESUME','PAYMENT_PRODUCT_RESUME')),
    command_digest TEXT NOT NULL CHECK(command_digest ~ '^[0-9a-f]{64}$'),
    snapshot_digest TEXT NOT NULL CHECK(snapshot_digest ~ '^[0-9a-f]{64}$'),
    domain_revision TEXT NOT NULL CHECK(domain_revision ~ '^[1-9][0-9]*:[0-9a-f]{64}$'),
    approval_epoch BIGINT NOT NULL CHECK(approval_epoch>0),
    claimed_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp()
);
CREATE FUNCTION public.reject_billing_approval_proof_claim_mutation() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
    RAISE EXCEPTION 'Billing approval proof claim is immutable' USING ERRCODE='55000';
END $$;
CREATE TRIGGER billing_approval_proof_claims_immutable
BEFORE UPDATE OR DELETE OR TRUNCATE ON public.billing_approval_proof_claims
FOR EACH STATEMENT EXECUTE FUNCTION public.reject_billing_approval_proof_claim_mutation();
REVOKE ALL ON public.billing_approval_proof_claims FROM PUBLIC;
GRANT SELECT,INSERT ON public.billing_approval_proof_claims TO billing_service;
SET ROLE billing_service;
