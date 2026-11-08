SET ROLE billing_service;
DO $$
DECLARE is_superuser BOOLEAN;
BEGIN
    SELECT rolsuper INTO is_superuser FROM pg_catalog.pg_roles WHERE rolname=session_user;
    IF session_user<>'billing_migrator' AND NOT is_superuser THEN
        RAISE EXCEPTION '000066 requires the dedicated billing_migrator deployment identity';
    END IF;
    IF NOT is_superuser AND current_user<>'billing_service' THEN
        RAISE EXCEPTION '000066 must run with MIGRATION_ROLE=billing_service';
    END IF;
    IF NOT pg_catalog.pg_has_role(session_user,'billing_schema_owner','MEMBER')
       OR pg_catalog.pg_has_role('billing_service','billing_schema_owner','MEMBER') THEN
        RAISE EXCEPTION '000066 schema owner must remain separate from runtime';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_attribute
                   WHERE attrelid='public.payment_order_repairs'::regclass AND attname='facts_digest' AND NOT attisdropped) THEN
        RAISE EXCEPTION '000066 requires the approval linkage columns from 000065';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint
                   WHERE conname='billing_approval_proof_claims_command_type_check'
                     AND conrelid='public.billing_approval_proof_claims'::regclass) THEN
        RAISE EXCEPTION '000066 requires the immutable proof claim table from 000055';
    END IF;
END $$;

SET ROLE billing_schema_owner;
-- The proof claim table is widened once per new proof-era command (000059
-- entitlement correction, 000062 ledger repair, 000064 receivable write-off).
-- The payment financial repair command now owns its own closed proof era too,
-- so the claim allowlist is widened again by replacing the constraint; the table
-- itself stays append-only and immutable.
ALTER TABLE public.billing_approval_proof_claims
    DROP CONSTRAINT billing_approval_proof_claims_command_type_check;
ALTER TABLE public.billing_approval_proof_claims
    ADD CONSTRAINT billing_approval_proof_claims_command_type_check
    CHECK (command_type IN ('WALLET_ADJUSTMENT','PAYMENT_PROVIDER_RESUME','PAYMENT_PRODUCT_RESUME','ENTITLEMENT_COMPONENT_ADJUSTMENT','LEDGER_REPAIR','RECEIVABLE_WRITE_OFF','PAYMENT_FINANCIAL_REPAIR'));
SET ROLE billing_service;
