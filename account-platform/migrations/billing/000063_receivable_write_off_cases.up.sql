SET ROLE billing_service;
DO $$
DECLARE is_superuser BOOLEAN;
BEGIN
    SELECT rolsuper INTO is_superuser FROM pg_catalog.pg_roles WHERE rolname=session_user;
    IF session_user<>'billing_migrator' AND NOT is_superuser THEN
        RAISE EXCEPTION '000063 requires the dedicated billing_migrator deployment identity';
    END IF;
    IF NOT is_superuser AND current_user<>'billing_service' THEN
        RAISE EXCEPTION '000063 must run with MIGRATION_ROLE=billing_service';
    END IF;
    IF NOT pg_catalog.pg_has_role(session_user,'billing_schema_owner','MEMBER')
       OR pg_catalog.pg_has_role('billing_service','billing_schema_owner','MEMBER') THEN
        RAISE EXCEPTION '000063 schema owner must remain separate from runtime';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM public.billing_approval_command_activations
                   WHERE command_type='RECEIVABLE_WRITE_OFF' AND policy_version=1
                     AND snapshot_schema_version=1 AND state='INACTIVE') THEN
        RAISE EXCEPTION '000063 requires the deployed inactive receivable write-off activation row';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint
                   WHERE conname='receivable_allocations_source_type_check'
                     AND conrelid='public.receivable_allocations'::regclass
                     AND pg_catalog.pg_get_constraintdef(oid) LIKE '%APPROVED_WRITE_OFF%') THEN
        RAISE EXCEPTION '000063 requires the receivable allocation contract from 000025';
    END IF;
END $$;

SET ROLE billing_service;
-- The immutable case table is owned by the schema owner, so the runtime owner of
-- the referenced receivable/ledger tables must grant it REFERENCES explicitly
-- (same pattern as 000056/000058/000061); ownership never moves.
GRANT REFERENCES ON public.receivables,public.receivable_allocations,public.journal_entries,public.wallets TO billing_schema_owner;

SET ROLE billing_schema_owner;
-- GSA-26 receivable write-off: one immutable case per approved write-off. The
-- audit row is the result-first reference and the per-approval linearization
-- point (approval_request_id, authorization_event_id, execution_key_digest,
-- journal_entry_id and allocation_id are unique). The table never stores
-- provider text, personal data or free-form input.
CREATE TABLE public.receivable_write_off_cases (
    id UUID PRIMARY KEY,
    approval_request_id UUID NOT NULL UNIQUE,
    authorization_event_id UUID NOT NULL UNIQUE,
    approval_epoch BIGINT NOT NULL CHECK(approval_epoch>0),
    execution_key_digest TEXT NOT NULL UNIQUE CHECK(execution_key_digest ~ '^[0-9a-f]{64}$'),
    receivable_id UUID NOT NULL REFERENCES public.receivables(id),
    wallet_id UUID NOT NULL REFERENCES public.wallets(id),
    amount_microcredits BIGINT NOT NULL CHECK(amount_microcredits>0),
    receivable_version_before BIGINT NOT NULL CHECK(receivable_version_before>0),
    receivable_version_after BIGINT NOT NULL CHECK(receivable_version_after=receivable_version_before+1),
    status_after TEXT NOT NULL CHECK(status_after IN ('PARTIALLY_SETTLED','WRITTEN_OFF')),
    facts_digest TEXT NOT NULL CHECK(facts_digest ~ '^[0-9a-f]{64}$'),
    journal_entry_id UUID NOT NULL UNIQUE REFERENCES public.journal_entries(id),
    allocation_id UUID NOT NULL UNIQUE REFERENCES public.receivable_allocations(id),
    state TEXT NOT NULL CHECK(state IN ('EXECUTED')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp()
);
CREATE FUNCTION public.reject_receivable_write_off_case_mutation() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
    RAISE EXCEPTION 'receivable write-off case is immutable' USING ERRCODE='55000';
END $$;
CREATE TRIGGER receivable_write_off_cases_immutable
BEFORE UPDATE OR DELETE OR TRUNCATE ON public.receivable_write_off_cases
FOR EACH STATEMENT EXECUTE FUNCTION public.reject_receivable_write_off_case_mutation();
REVOKE ALL ON public.receivable_write_off_cases FROM PUBLIC;
GRANT SELECT,INSERT ON public.receivable_write_off_cases TO billing_service;
SET ROLE billing_service;
