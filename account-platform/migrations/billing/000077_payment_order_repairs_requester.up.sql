SET ROLE billing_service;
DO $$
DECLARE is_superuser BOOLEAN;
BEGIN
    SELECT rolsuper INTO is_superuser FROM pg_catalog.pg_roles WHERE rolname=session_user;
    IF session_user<>'billing_migrator' AND NOT is_superuser THEN
        RAISE EXCEPTION '000077 requires the dedicated billing_migrator deployment identity';
    END IF;
    IF NOT is_superuser AND current_user<>'billing_service' THEN
        RAISE EXCEPTION '000077 must run with MIGRATION_ROLE=billing_service';
    END IF;
    IF NOT pg_catalog.pg_has_role(session_user,'billing_schema_owner','MEMBER')
       OR pg_catalog.pg_has_role('billing_service','billing_schema_owner','MEMBER') THEN
        RAISE EXCEPTION '000077 schema owner must remain separate from runtime';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM public.billing_approval_command_activations
                   WHERE command_type='PAYMENT_FINANCIAL_REPAIR' AND policy_version=1
                     AND snapshot_schema_version=1 AND state='INACTIVE') THEN
        RAISE EXCEPTION '000077 requires the deployed inactive payment financial repair activation row';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint
                   WHERE conname='payment_order_repairs_approval_linkage_check'
                     AND conrelid='public.payment_order_repairs'::regclass) THEN
        RAISE EXCEPTION '000077 requires the approval linkage appended by 000065';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_attribute attribute
               WHERE attribute.attrelid='public.payment_order_repairs'::regclass
                 AND attribute.attname='requester_user_id' AND NOT attribute.attisdropped) THEN
        RAISE EXCEPTION '000077 requester column must not pre-exist';
    END IF;
END $$;

SET ROLE billing_service;
-- GSA-27/28 follow-up (user decision 2026-09-21): an approval-driven payment
-- financial repair must record BOTH sides of the independent approval - the
-- approving executor in actor_id (000014/000065) and now the requester frozen
-- on the approval wire. The column stays nullable so the pre-existing
-- operational repair path keeps writing rows without approval facts; the two
-- checks below make the approver/requester pair all-or-none with the approval
-- linkage and keep the two identities distinct, exactly like
-- payment_provider_resume_executions (000040).
--
-- The audited table is owned by the runtime, while adding constraints needs
-- CREATE on the schema: the schema owner lends that privilege for the duration
-- of this migration only and revokes it again before it ends.
SET ROLE billing_schema_owner;
GRANT CREATE ON SCHEMA public TO billing_service;
SET ROLE billing_service;
ALTER TABLE public.payment_order_repairs
    ADD COLUMN requester_user_id UUID,
    ADD CONSTRAINT payment_order_repairs_requester_pair_check
        CHECK ((requester_user_id IS NOT NULL) = (approval_request_id IS NOT NULL)),
    ADD CONSTRAINT payment_order_repairs_requester_distinct_check
        CHECK (requester_user_id IS NULL OR requester_user_id <> actor_id);
SET ROLE billing_schema_owner;
REVOKE CREATE ON SCHEMA public FROM billing_service;
SET ROLE billing_service;
