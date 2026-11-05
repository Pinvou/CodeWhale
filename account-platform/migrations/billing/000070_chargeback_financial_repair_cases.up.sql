SET ROLE billing_service;
DO $$
DECLARE is_superuser BOOLEAN;
BEGIN
    SELECT rolsuper INTO is_superuser FROM pg_catalog.pg_roles WHERE rolname=session_user;
    IF session_user<>'billing_migrator' AND NOT is_superuser THEN
        RAISE EXCEPTION '000070 requires the dedicated billing_migrator deployment identity';
    END IF;
    IF NOT is_superuser AND current_user<>'billing_service' THEN
        RAISE EXCEPTION '000070 must run with MIGRATION_ROLE=billing_service';
    END IF;
    IF NOT pg_catalog.pg_has_role(session_user,'billing_schema_owner','MEMBER')
       OR pg_catalog.pg_has_role('billing_service','billing_schema_owner','MEMBER') THEN
        RAISE EXCEPTION '000070 schema owner must remain separate from runtime';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM public.billing_approval_command_activations
                   WHERE command_type='CHARGEBACK_FINANCIAL_REPAIR' AND policy_version=1
                     AND snapshot_schema_version=1 AND state='INACTIVE') THEN
        RAISE EXCEPTION '000070 requires the deployed inactive chargeback financial repair activation row';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint
                   WHERE conname='receivables_source_type_check'
                     AND conrelid='public.receivables'::regclass
                     AND pg_catalog.pg_get_constraintdef(oid) LIKE '%CHARGEBACK%') THEN
        RAISE EXCEPTION '000070 requires the receivable source enum from 000025';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint
                   WHERE conname='payment_anomaly_cases_financial_action_status_check'
                     AND conrelid='public.payment_anomaly_cases'::regclass
                     AND pg_catalog.pg_get_constraintdef(oid) LIKE '%APPLIED%') THEN
        RAISE EXCEPTION '000070 requires the payment anomaly lifecycle from 000029/000030';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_class
                   WHERE relname='chargeback_cases' AND relkind='r') THEN
        RAISE EXCEPTION '000070 requires the chargeback case model from 000025';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint
                   WHERE conname='chargeback_cases_status_check'
                     AND conrelid='public.chargeback_cases'::regclass
                     AND pg_catalog.pg_get_constraintdef(oid) LIKE '%REVIEW_REQUIRED%') THEN
        RAISE EXCEPTION '000070 requires the reviewed chargeback lifecycle from 000029';
    END IF;
END $$;

SET ROLE billing_service;
-- The immutable case table is owned by the schema owner, so the runtime owner of
-- the referenced chargeback/anomaly/ledger tables must grant it REFERENCES
-- explicitly (same pattern as 000056/000058/000061/000063/000067).
GRANT REFERENCES ON public.chargeback_cases,public.payment_anomaly_cases,public.wallets,public.entitlement_packages,
    public.payment_evidence,public.journal_entries,public.receivables TO billing_schema_owner;

SET ROLE billing_schema_owner;
-- GSA-32 chargeback financial repair: one immutable case per compensated
-- chargeback. The chargeback case id is the linearization point, and the
-- journal/receivable identities are unique so a second execution for the same
-- case can only read the existing case back. The table never stores provider
-- text or personal data.
CREATE TABLE public.chargeback_financial_repair_cases (
    id UUID PRIMARY KEY,
    approval_request_id UUID NOT NULL UNIQUE,
    authorization_event_id UUID NOT NULL UNIQUE,
    approval_epoch BIGINT NOT NULL CHECK(approval_epoch>0),
    execution_key_digest TEXT NOT NULL UNIQUE CHECK(execution_key_digest ~ '^[0-9a-f]{64}$'),
    chargeback_case_id UUID NOT NULL UNIQUE REFERENCES public.chargeback_cases(id),
    anomaly_case_id UUID NOT NULL REFERENCES public.payment_anomaly_cases(id),
    wallet_id UUID NOT NULL REFERENCES public.wallets(id),
    package_id UUID NOT NULL REFERENCES public.entitlement_packages(id),
    payment_evidence_id UUID NOT NULL REFERENCES public.payment_evidence(id),
    base_shortfall_microcredits BIGINT NOT NULL CHECK(base_shortfall_microcredits>0),
    component_digest TEXT NOT NULL CHECK(component_digest ~ '^[0-9a-f]{64}$'),
    facts_digest TEXT NOT NULL CHECK(facts_digest ~ '^[0-9a-f]{64}$'),
    journal_entry_id UUID NOT NULL UNIQUE REFERENCES public.journal_entries(id),
    receivable_id UUID NOT NULL UNIQUE REFERENCES public.receivables(id),
    state TEXT NOT NULL CHECK(state IN ('EXECUTED')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp()
);
CREATE FUNCTION public.reject_chargeback_financial_repair_case_mutation() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
    RAISE EXCEPTION 'chargeback financial repair case is immutable' USING ERRCODE='55000';
END $$;
CREATE TRIGGER chargeback_financial_repair_cases_immutable
BEFORE UPDATE OR DELETE OR TRUNCATE ON public.chargeback_financial_repair_cases
FOR EACH STATEMENT EXECUTE FUNCTION public.reject_chargeback_financial_repair_case_mutation();
REVOKE ALL ON public.chargeback_financial_repair_cases FROM PUBLIC;
GRANT SELECT,INSERT ON public.chargeback_financial_repair_cases TO billing_service;
SET ROLE billing_service;
