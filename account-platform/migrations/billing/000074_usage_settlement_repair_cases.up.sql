SET ROLE billing_service;
DO $$
DECLARE is_superuser BOOLEAN;
BEGIN
    SELECT rolsuper INTO is_superuser FROM pg_catalog.pg_roles WHERE rolname=session_user;
    IF session_user<>'billing_migrator' AND NOT is_superuser THEN
        RAISE EXCEPTION '000074 requires the dedicated billing_migrator deployment identity';
    END IF;
    IF NOT is_superuser AND current_user<>'billing_service' THEN
        RAISE EXCEPTION '000074 must run with MIGRATION_ROLE=billing_service';
    END IF;
    IF NOT pg_catalog.pg_has_role(session_user,'billing_schema_owner','MEMBER')
       OR pg_catalog.pg_has_role('billing_service','billing_schema_owner','MEMBER') THEN
        RAISE EXCEPTION '000074 schema owner must remain separate from runtime';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM public.billing_approval_command_activations
                   WHERE command_type='USAGE_SETTLEMENT_REPAIR' AND policy_version=1
                     AND snapshot_schema_version=1 AND state='INACTIVE') THEN
        RAISE EXCEPTION '000074 requires the deployed inactive usage settlement repair activation row';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_class
                   WHERE relname='usage_loss_reviews' AND relkind='r') THEN
        RAISE EXCEPTION '000074 requires the usage loss review model from 000016';
    END IF;
END $$;

SET ROLE billing_service;
-- The immutable case table is owned by the schema owner, so the runtime owner of
-- the referenced usage/ledger tables must grant it REFERENCES explicitly (same
-- pattern as 000056/000058/000061/000063/000067/000070/000072).
GRANT REFERENCES ON public.usage_holds,public.usage_loss_reviews,public.wallets,public.journal_entries,
    public.receivables TO billing_schema_owner;

SET ROLE billing_schema_owner;
-- GSA-36 usage settlement repair: one immutable补结算 case per usage request. The
-- usage request id is the linearization point and the journal/receivable
-- identities are unique, so a second execution can only read the existing case.
CREATE TABLE public.usage_settlement_repair_cases (
    id UUID PRIMARY KEY,
    approval_request_id UUID NOT NULL UNIQUE,
    authorization_event_id UUID NOT NULL UNIQUE,
    approval_epoch BIGINT NOT NULL CHECK(approval_epoch>0),
    execution_key_digest TEXT NOT NULL UNIQUE CHECK(execution_key_digest ~ '^[0-9a-f]{64}$'),
    usage_request_id UUID NOT NULL UNIQUE,
    hold_id UUID NOT NULL REFERENCES public.usage_holds(id),
    loss_review_id UUID UNIQUE REFERENCES public.usage_loss_reviews(id),
    wallet_id UUID NOT NULL REFERENCES public.wallets(id),
    evidence_kind TEXT NOT NULL CHECK(evidence_kind IN ('LOSS_REVIEW')),
    expected_microcredits BIGINT NOT NULL CHECK(expected_microcredits>0),
    settled_microcredits BIGINT NOT NULL CHECK(settled_microcredits>=0),
    overage_microcredits BIGINT NOT NULL CHECK(overage_microcredits>=0),
    allocations_digest TEXT NOT NULL CHECK(allocations_digest ~ '^[0-9a-f]{64}$'),
    facts_digest TEXT NOT NULL CHECK(facts_digest ~ '^[0-9a-f]{64}$'),
    journal_entry_id UUID NOT NULL UNIQUE REFERENCES public.journal_entries(id),
    receivable_id UUID UNIQUE REFERENCES public.receivables(id),
    state TEXT NOT NULL CHECK(state IN ('EXECUTED')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    CHECK (settled_microcredits+overage_microcredits=expected_microcredits)
);
CREATE FUNCTION public.reject_usage_settlement_repair_case_mutation() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
    RAISE EXCEPTION 'usage settlement repair case is immutable' USING ERRCODE='55000';
END $$;
CREATE TRIGGER usage_settlement_repair_cases_immutable
BEFORE UPDATE OR DELETE OR TRUNCATE ON public.usage_settlement_repair_cases
FOR EACH STATEMENT EXECUTE FUNCTION public.reject_usage_settlement_repair_case_mutation();
REVOKE ALL ON public.usage_settlement_repair_cases FROM PUBLIC;
GRANT SELECT,INSERT ON public.usage_settlement_repair_cases TO billing_service;
SET ROLE billing_service;
