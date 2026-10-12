SET ROLE billing_service;
DO $$
DECLARE is_superuser BOOLEAN;
BEGIN
    SELECT rolsuper INTO is_superuser FROM pg_catalog.pg_roles WHERE rolname=session_user;
    IF session_user<>'billing_migrator' AND NOT is_superuser THEN
        RAISE EXCEPTION '000059 requires the dedicated billing_migrator deployment identity';
    END IF;
    IF NOT is_superuser AND current_user<>'billing_service' THEN
        RAISE EXCEPTION '000059 must run with MIGRATION_ROLE=billing_service';
    END IF;
    IF NOT pg_catalog.pg_has_role(session_user,'billing_schema_owner','MEMBER')
       OR pg_catalog.pg_has_role('billing_service','billing_schema_owner','MEMBER') THEN
        RAISE EXCEPTION '000059 schema owner must remain separate from runtime';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM public.billing_approval_command_activations
                   WHERE command_type='ENTITLEMENT_COMPONENT_ADJUSTMENT' AND policy_version=2
                     AND snapshot_schema_version=2 AND state='INACTIVE') THEN
        RAISE EXCEPTION '000059 requires the appended inactive entitlement activation row';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint
                   WHERE conname='billing_approval_proof_claims_command_type_check'
                     AND conrelid='public.billing_approval_proof_claims'::regclass) THEN
        RAISE EXCEPTION '000059 requires the immutable proof claim table from 000055';
    END IF;
END $$;

SET ROLE billing_schema_owner;
-- The proof claim table was created while only the three original Billing
-- commands had proofs. The entitlement correction command now owns its own
-- closed proof era, so the claim allowlist is widened by replacing the
-- constraint; the claim table itself stays append-only and immutable.
ALTER TABLE public.billing_approval_proof_claims
    DROP CONSTRAINT billing_approval_proof_claims_command_type_check;
ALTER TABLE public.billing_approval_proof_claims
    ADD CONSTRAINT billing_approval_proof_claims_command_type_check
    CHECK (command_type IN ('WALLET_ADJUSTMENT','PAYMENT_PROVIDER_RESUME','PAYMENT_PRODUCT_RESUME','ENTITLEMENT_COMPONENT_ADJUSTMENT'));
SET ROLE billing_service;
