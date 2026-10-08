SET ROLE billing_service;
DO $$
DECLARE is_superuser BOOLEAN;
BEGIN
    SELECT rolsuper INTO is_superuser FROM pg_catalog.pg_roles WHERE rolname=session_user;
    IF session_user<>'billing_migrator' AND NOT is_superuser THEN
        RAISE EXCEPTION '000072 requires the dedicated billing_migrator deployment identity';
    END IF;
    IF NOT is_superuser AND current_user<>'billing_service' THEN
        RAISE EXCEPTION '000072 must run with MIGRATION_ROLE=billing_service';
    END IF;
    IF NOT pg_catalog.pg_has_role(session_user,'billing_schema_owner','MEMBER')
       OR pg_catalog.pg_has_role('billing_service','billing_schema_owner','MEMBER') THEN
        RAISE EXCEPTION '000072 schema owner must remain separate from runtime';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM public.billing_approval_command_activations
                   WHERE command_type='RECONCILIATION_FINANCIAL_REPAIR' AND policy_version=1
                     AND snapshot_schema_version=1 AND state='INACTIVE') THEN
        RAISE EXCEPTION '000072 requires the deployed inactive reconciliation financial repair activation row';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_class
                   WHERE relname='reconciliation_cases' AND relkind='r') THEN
        RAISE EXCEPTION '000072 requires the reconciliation case model from 000004';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint
                   WHERE conname='reconciliation_cases_status_check'
                     AND conrelid='public.reconciliation_cases'::regclass
                     AND pg_catalog.pg_get_constraintdef(oid) LIKE '%RESOLVED%') THEN
        RAISE EXCEPTION '000072 requires the reconciliation case lifecycle from 000004';
    END IF;
END $$;

SET ROLE billing_service;
-- The immutable case table is owned by the schema owner, so the runtime owner of
-- the referenced reconciliation/evidence tables must grant it REFERENCES
-- explicitly (same pattern as 000056/000058/000061/000063/000067/000070).
GRANT REFERENCES ON public.reconciliation_cases,public.reconciliation_runs,public.payment_evidence
    TO billing_schema_owner;

SET ROLE billing_schema_owner;
-- GSA-34 reconciliation financial repair: one immutable closure per reconciled
-- difference. The repository table records what was closed and the recheck
-- result; it deliberately carries no journal or receivable identity because the
-- first release has zero accounting effect.
CREATE TABLE public.reconciliation_repair_cases (
    id UUID PRIMARY KEY,
    approval_request_id UUID NOT NULL UNIQUE,
    authorization_event_id UUID NOT NULL UNIQUE,
    approval_epoch BIGINT NOT NULL CHECK(approval_epoch>0),
    execution_key_digest TEXT NOT NULL UNIQUE CHECK(execution_key_digest ~ '^[0-9a-f]{64}$'),
    reconciliation_run_id UUID NOT NULL REFERENCES public.reconciliation_runs(id),
    reconciliation_case_id UUID NOT NULL UNIQUE REFERENCES public.reconciliation_cases(id),
    channel TEXT NOT NULL CHECK(btrim(channel)<>''),
    business_date DATE NOT NULL,
    merchant_order_no TEXT NOT NULL CHECK(btrim(merchant_order_no)<>''),
    amount_fen BIGINT NOT NULL CHECK(amount_fen>0),
    currency TEXT NOT NULL CHECK(btrim(currency)<>''),
    payment_evidence_id UUID NOT NULL REFERENCES public.payment_evidence(id),
    provider_snapshot_digest TEXT NOT NULL CHECK(provider_snapshot_digest ~ '^[0-9a-f]{64}$'),
    facts_digest TEXT NOT NULL CHECK(facts_digest ~ '^[0-9a-f]{64}$'),
    resolution_code TEXT NOT NULL CHECK(resolution_code IN ('PROVIDER_REFUND_CONFIRMED')),
    recheck_case_count BIGINT NOT NULL CHECK(recheck_case_count=0),
    state TEXT NOT NULL CHECK(state IN ('EXECUTED')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp()
);
CREATE FUNCTION public.reject_reconciliation_repair_case_mutation() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
    RAISE EXCEPTION 'reconciliation repair case is immutable' USING ERRCODE='55000';
END $$;
CREATE TRIGGER reconciliation_repair_cases_immutable
BEFORE UPDATE OR DELETE OR TRUNCATE ON public.reconciliation_repair_cases
FOR EACH STATEMENT EXECUTE FUNCTION public.reject_reconciliation_repair_case_mutation();
REVOKE ALL ON public.reconciliation_repair_cases FROM PUBLIC;
GRANT SELECT,INSERT ON public.reconciliation_repair_cases TO billing_service;
SET ROLE billing_service;
