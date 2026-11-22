SET ROLE billing_service;
DO $$
DECLARE is_superuser BOOLEAN;
BEGIN
    SELECT rolsuper INTO is_superuser FROM pg_catalog.pg_roles WHERE rolname=session_user;
    IF session_user<>'billing_migrator' AND NOT is_superuser THEN
        RAISE EXCEPTION '000068 requires the dedicated billing_migrator deployment identity';
    END IF;
    IF NOT is_superuser AND current_user<>'billing_service' THEN
        RAISE EXCEPTION '000068 must run with MIGRATION_ROLE=billing_service';
    END IF;
    IF NOT pg_catalog.pg_has_role(session_user,'billing_schema_owner','MEMBER')
       OR pg_catalog.pg_has_role('billing_service','billing_schema_owner','MEMBER') THEN
        RAISE EXCEPTION '000068 schema owner must remain separate from runtime';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_class
                   WHERE relname='refund_exception_repair_cases' AND relkind='r') THEN
        RAISE EXCEPTION '000068 requires the immutable refund repair case table from 000067';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint
                   WHERE conname='billing_approval_proof_claims_command_type_check'
                     AND conrelid='public.billing_approval_proof_claims'::regclass) THEN
        RAISE EXCEPTION '000068 requires the immutable proof claim table from 000055';
    END IF;
END $$;

SET ROLE billing_schema_owner;
-- The proof claim allowlist is widened once per proof-era command (000059
-- entitlement correction, 000062 ledger repair, 000064 receivable write-off,
-- 000066 payment financial repair). The refund exception repair command now owns
-- its own closed proof era too; the table itself stays append-only.
ALTER TABLE public.billing_approval_proof_claims
    DROP CONSTRAINT billing_approval_proof_claims_command_type_check;
ALTER TABLE public.billing_approval_proof_claims
    ADD CONSTRAINT billing_approval_proof_claims_command_type_check
    CHECK (command_type IN ('WALLET_ADJUSTMENT','PAYMENT_PROVIDER_RESUME','PAYMENT_PRODUCT_RESUME','ENTITLEMENT_COMPONENT_ADJUSTMENT','LEDGER_REPAIR','RECEIVABLE_WRITE_OFF','PAYMENT_FINANCIAL_REPAIR','REFUND_EXCEPTION_REPAIR'));
SET ROLE billing_service;
