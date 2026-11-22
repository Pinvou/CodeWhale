SET ROLE billing_service;
DO $$
DECLARE is_superuser BOOLEAN;
BEGIN
    SELECT rolsuper INTO is_superuser FROM pg_catalog.pg_roles WHERE rolname=session_user;
    IF session_user<>'billing_migrator' AND NOT is_superuser THEN
        RAISE EXCEPTION '000065 requires the dedicated billing_migrator deployment identity';
    END IF;
    IF NOT is_superuser AND current_user<>'billing_service' THEN
        RAISE EXCEPTION '000065 must run with MIGRATION_ROLE=billing_service';
    END IF;
    IF NOT pg_catalog.pg_has_role(session_user,'billing_schema_owner','MEMBER')
       OR pg_catalog.pg_has_role('billing_service','billing_schema_owner','MEMBER') THEN
        RAISE EXCEPTION '000065 schema owner must remain separate from runtime';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM public.billing_approval_command_activations
                   WHERE command_type='PAYMENT_FINANCIAL_REPAIR' AND policy_version=1
                     AND snapshot_schema_version=1 AND state='INACTIVE') THEN
        RAISE EXCEPTION '000065 requires the deployed inactive payment financial repair activation row';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint
                   WHERE conname='payment_order_repairs_transaction_order_fk'
                     AND conrelid='public.payment_order_repairs'::regclass) THEN
        RAISE EXCEPTION '000065 requires the audited repair table from 000014/000015/000024';
    END IF;
END $$;

SET ROLE billing_service;
-- GSA-28 payment financial repair: the audited repair row stays the single
-- per-transaction linearization point (payment_transaction_id and
-- entitlement_package_id remain unique). The approval linkage is appended as
-- nullable columns so the existing operational path keeps writing rows with
-- NULL approval facts, which readers must keep distinguishing from an
-- approval-driven repair.
--
-- The audited table is owned by the runtime, while adding constraints needs
-- CREATE on the schema: the schema owner therefore lends that privilege for the
-- duration of this migration only and revokes it again before it ends.
SET ROLE billing_schema_owner;
GRANT CREATE ON SCHEMA public TO billing_service;
SET ROLE billing_service;
ALTER TABLE public.payment_order_repairs
    ADD COLUMN approval_request_id UUID,
    ADD COLUMN authorization_event_id UUID,
    ADD COLUMN approval_epoch BIGINT,
    ADD COLUMN execution_key_digest TEXT,
    ADD COLUMN facts_digest TEXT,
    ADD CONSTRAINT payment_order_repairs_approval_epoch_check
        CHECK (approval_epoch IS NULL OR approval_epoch > 0),
    ADD CONSTRAINT payment_order_repairs_execution_key_digest_check
        CHECK (execution_key_digest IS NULL OR execution_key_digest ~ '^[0-9a-f]{64}$'),
    ADD CONSTRAINT payment_order_repairs_facts_digest_check
        CHECK (facts_digest IS NULL OR facts_digest ~ '^[0-9a-f]{64}$'),
    ADD CONSTRAINT payment_order_repairs_approval_linkage_check
        CHECK (num_nonnulls(approval_request_id,authorization_event_id,approval_epoch,execution_key_digest,facts_digest) IN (0,5)),
    -- PostgreSQL unique constraints treat NULLs as distinct, so these three
    -- constraints are exactly the design's "unique when present" rule while the
    -- existing operational rows keep all five columns NULL.
    ADD CONSTRAINT payment_order_repairs_approval_request_key UNIQUE (approval_request_id),
    ADD CONSTRAINT payment_order_repairs_authorization_event_key UNIQUE (authorization_event_id),
    ADD CONSTRAINT payment_order_repairs_execution_key_digest_key UNIQUE (execution_key_digest);
SET ROLE billing_schema_owner;
REVOKE CREATE ON SCHEMA public FROM billing_service;
SET ROLE billing_service;
