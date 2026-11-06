SET ROLE billing_service;
DO $$
DECLARE is_superuser BOOLEAN;
BEGIN
    SELECT rolsuper INTO is_superuser FROM pg_catalog.pg_roles WHERE rolname=session_user;
    IF session_user<>'billing_migrator' AND NOT is_superuser THEN
        RAISE EXCEPTION '000067 requires the dedicated billing_migrator deployment identity';
    END IF;
    IF NOT is_superuser AND current_user<>'billing_service' THEN
        RAISE EXCEPTION '000067 must run with MIGRATION_ROLE=billing_service';
    END IF;
    IF NOT pg_catalog.pg_has_role(session_user,'billing_schema_owner','MEMBER')
       OR pg_catalog.pg_has_role('billing_service','billing_schema_owner','MEMBER') THEN
        RAISE EXCEPTION '000067 schema owner must remain separate from runtime';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM public.billing_approval_command_activations
                   WHERE command_type='REFUND_EXCEPTION_REPAIR' AND policy_version=1
                     AND snapshot_schema_version=1 AND state='INACTIVE') THEN
        RAISE EXCEPTION '000067 requires the deployed inactive refund exception repair activation row';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint
                   WHERE conname='receivables_source_type_check'
                     AND conrelid='public.receivables'::regclass
                     AND pg_catalog.pg_get_constraintdef(oid) LIKE '%FORCED_REFUND%') THEN
        RAISE EXCEPTION '000067 requires the widened receivable enum from 000060';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint
                   WHERE conname='payment_anomaly_cases_financial_action_status_check'
                     AND conrelid='public.payment_anomaly_cases'::regclass
                     AND pg_catalog.pg_get_constraintdef(oid) LIKE '%APPLIED%') THEN
        RAISE EXCEPTION '000067 requires the payment anomaly lifecycle from 000029/000030';
    END IF;
END $$;

SET ROLE billing_service;
-- The immutable case table is owned by the schema owner, so the runtime owner of
-- the referenced refund/anomaly/ledger tables must grant it REFERENCES
-- explicitly (same pattern as 000056/000058/000061/000063).
GRANT REFERENCES ON public.refunds,public.payment_anomaly_cases,public.wallets,public.entitlement_packages,
    public.payment_evidence,public.journal_entries,public.receivables TO billing_schema_owner;

SET ROLE billing_schema_owner;
-- GSA-30 refund exception repair: one immutable case per repaired refund. The
-- refund id is the linearization point, and the journal/receivable identities
-- are unique so a second execution for the same refund can only read the
-- existing case back. The table never stores provider text or personal data.
CREATE TABLE public.refund_exception_repair_cases (
    id UUID PRIMARY KEY,
    approval_request_id UUID NOT NULL UNIQUE,
    authorization_event_id UUID NOT NULL UNIQUE,
    approval_epoch BIGINT NOT NULL CHECK(approval_epoch>0),
    execution_key_digest TEXT NOT NULL UNIQUE CHECK(execution_key_digest ~ '^[0-9a-f]{64}$'),
    refund_id UUID NOT NULL UNIQUE REFERENCES public.refunds(id),
    anomaly_case_id UUID NOT NULL REFERENCES public.payment_anomaly_cases(id),
    wallet_id UUID NOT NULL REFERENCES public.wallets(id),
    package_id UUID NOT NULL REFERENCES public.entitlement_packages(id),
    revoked_microcredits BIGINT NOT NULL CHECK(revoked_microcredits>=0),
    shortfall_microcredits BIGINT NOT NULL CHECK(shortfall_microcredits>=0),
    payment_evidence_id UUID NOT NULL REFERENCES public.payment_evidence(id),
    facts_digest TEXT NOT NULL CHECK(facts_digest ~ '^[0-9a-f]{64}$'),
    journal_entry_id UUID NOT NULL UNIQUE REFERENCES public.journal_entries(id),
    receivable_id UUID UNIQUE REFERENCES public.receivables(id),
    state TEXT NOT NULL CHECK(state IN ('EXECUTED')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    CHECK (revoked_microcredits+shortfall_microcredits>0)
);
CREATE FUNCTION public.reject_refund_exception_repair_case_mutation() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
    RAISE EXCEPTION 'refund exception repair case is immutable' USING ERRCODE='55000';
END $$;
CREATE TRIGGER refund_exception_repair_cases_immutable
BEFORE UPDATE OR DELETE OR TRUNCATE ON public.refund_exception_repair_cases
FOR EACH STATEMENT EXECUTE FUNCTION public.reject_refund_exception_repair_case_mutation();
REVOKE ALL ON public.refund_exception_repair_cases FROM PUBLIC;
GRANT SELECT,INSERT ON public.refund_exception_repair_cases TO billing_service;
SET ROLE billing_service;
