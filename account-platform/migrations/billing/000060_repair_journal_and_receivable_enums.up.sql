SET ROLE billing_service;
DO $$
DECLARE is_superuser BOOLEAN;
BEGIN
    SELECT rolsuper INTO is_superuser FROM pg_catalog.pg_roles WHERE rolname=session_user;
    IF session_user<>'billing_migrator' AND NOT is_superuser THEN
        RAISE EXCEPTION '000060 requires the dedicated billing_migrator deployment identity';
    END IF;
    IF NOT is_superuser AND current_user<>'billing_service' THEN
        RAISE EXCEPTION '000060 must run with MIGRATION_ROLE=billing_service';
    END IF;
    IF NOT pg_catalog.pg_has_role(session_user,'billing_schema_owner','MEMBER')
       OR pg_catalog.pg_has_role('billing_service','billing_schema_owner','MEMBER') THEN
        RAISE EXCEPTION '000060 schema owner must remain separate from runtime';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint
                   WHERE conname='journal_entries_entry_type_check'
                     AND conrelid='public.journal_entries'::regclass)
       OR NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint
                   WHERE conname='receivables_source_type_check'
                     AND conrelid='public.receivables'::regclass) THEN
        RAISE EXCEPTION '000060 requires the existing journal/receivable checks';
    END IF;
    IF (SELECT pg_catalog.pg_get_userbyid(relowner) FROM pg_catalog.pg_class
        WHERE oid='public.journal_entries'::regclass) <> 'billing_service'
       OR (SELECT pg_catalog.pg_get_userbyid(relowner) FROM pg_catalog.pg_class
        WHERE oid='public.receivables'::regclass) <> 'billing_service' THEN
        RAISE EXCEPTION '000060 repaired ledger tables must stay owned by billing_service';
    END IF;
END $$;

SET ROLE billing_service;
-- The journal white list was narrowed in 000025 while three writer paths
-- (verified payment repair credit, reviewed provider forced refund) still emit
-- their own closed values, and the forced refund review path creates a
-- BASE-shortfall receivable with its own source. Widening the two closed
-- enumerations restores those already-intended paths without rewriting any
-- history: the tables stay append-only and every value is owner-derived.
ALTER TABLE public.journal_entries DROP CONSTRAINT journal_entries_entry_type_check;
ALTER TABLE public.journal_entries ADD CONSTRAINT journal_entries_entry_type_check
    CHECK (entry_type IN ('RECHARGE','USAGE','REFUND','REVERSAL','ADJUSTMENT','EXPIRY','RECEIVABLE','CHARGEBACK','REPAIR_CREDIT','FORCED_REFUND'));
ALTER TABLE public.receivables DROP CONSTRAINT receivables_source_type_check;
ALTER TABLE public.receivables ADD CONSTRAINT receivables_source_type_check
    CHECK (source_type IN ('USAGE_OVERAGE','CHARGEBACK','FORCED_REFUND'));
SET ROLE billing_service;
