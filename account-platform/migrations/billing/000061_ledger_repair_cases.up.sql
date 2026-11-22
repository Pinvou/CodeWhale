SET ROLE billing_service;
DO $$
DECLARE is_superuser BOOLEAN;
BEGIN
    SELECT rolsuper INTO is_superuser FROM pg_catalog.pg_roles WHERE rolname=session_user;
    IF session_user<>'billing_migrator' AND NOT is_superuser THEN
        RAISE EXCEPTION '000061 requires the dedicated billing_migrator deployment identity';
    END IF;
    IF NOT is_superuser AND current_user<>'billing_service' THEN
        RAISE EXCEPTION '000061 must run with MIGRATION_ROLE=billing_service';
    END IF;
    IF NOT pg_catalog.pg_has_role(session_user,'billing_schema_owner','MEMBER')
       OR pg_catalog.pg_has_role('billing_service','billing_schema_owner','MEMBER') THEN
        RAISE EXCEPTION '000061 schema owner must remain separate from runtime';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint
                   WHERE conname='journal_entries_entry_type_check'
                     AND conrelid='public.journal_entries'::regclass
                     AND pg_catalog.pg_get_constraintdef(oid) LIKE '%REPAIR_CREDIT%'
                     AND pg_catalog.pg_get_constraintdef(oid) LIKE '%FORCED_REFUND%')
       OR NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint
                   WHERE conname='receivables_source_type_check'
                     AND conrelid='public.receivables'::regclass
                     AND pg_catalog.pg_get_constraintdef(oid) LIKE '%FORCED_REFUND%') THEN
        RAISE EXCEPTION '000061 requires the widened journal/receivable enums from 000060';
    END IF;
END $$;

SET ROLE billing_service;
-- The immutable case table is owned by the schema owner, so the runtime owner of
-- the referenced ledger tables must grant it REFERENCES explicitly (same
-- pattern as 000056/000058); ownership of the ledger tables never moves.
GRANT REFERENCES ON public.journal_entries,public.ledger_postings,public.ledger_accounts TO billing_schema_owner;

SET ROLE billing_schema_owner;
-- GSA-24 ledger repair: one immutable case per corrected posting. The audit row
-- is the result-first reference and the target uniqueness linearization point
-- (target_posting_id and journal_entry_id are unique), so a second execution for
-- the same posting can only read the existing case back. The table never stores
-- provider text, personal data or free-form input.
CREATE TABLE public.ledger_repair_cases (
    id UUID PRIMARY KEY,
    approval_request_id UUID NOT NULL UNIQUE,
    authorization_event_id UUID NOT NULL UNIQUE,
    approval_epoch BIGINT NOT NULL CHECK(approval_epoch>0),
    execution_key_digest TEXT NOT NULL UNIQUE CHECK(execution_key_digest ~ '^[0-9a-f]{64}$'),
    target_journal_entry_id UUID NOT NULL REFERENCES public.journal_entries(id),
    target_posting_id UUID NOT NULL UNIQUE REFERENCES public.ledger_postings(id),
    wrong_ledger_account_id UUID NOT NULL REFERENCES public.ledger_accounts(id),
    correct_ledger_account_id UUID NOT NULL REFERENCES public.ledger_accounts(id),
    entry_type TEXT NOT NULL CHECK(entry_type IN ('RECHARGE','USAGE','ADJUSTMENT','REFUND','EXPIRY','CHARGEBACK','REPAIR_CREDIT','FORCED_REFUND')),
    amount_microcredits BIGINT NOT NULL CHECK(amount_microcredits>0),
    posting_set_digest TEXT NOT NULL CHECK(posting_set_digest ~ '^[0-9a-f]{64}$'),
    facts_digest TEXT NOT NULL CHECK(facts_digest ~ '^[0-9a-f]{64}$'),
    journal_entry_id UUID NOT NULL UNIQUE REFERENCES public.journal_entries(id),
    state TEXT NOT NULL CHECK(state IN ('EXECUTED')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    CHECK (wrong_ledger_account_id <> correct_ledger_account_id)
);
CREATE FUNCTION public.reject_ledger_repair_case_mutation() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
    RAISE EXCEPTION 'ledger repair case is immutable' USING ERRCODE='55000';
END $$;
CREATE TRIGGER ledger_repair_cases_immutable
BEFORE UPDATE OR DELETE OR TRUNCATE ON public.ledger_repair_cases
FOR EACH STATEMENT EXECUTE FUNCTION public.reject_ledger_repair_case_mutation();
REVOKE ALL ON public.ledger_repair_cases FROM PUBLIC;
GRANT SELECT,INSERT ON public.ledger_repair_cases TO billing_service;
SET ROLE billing_service;
