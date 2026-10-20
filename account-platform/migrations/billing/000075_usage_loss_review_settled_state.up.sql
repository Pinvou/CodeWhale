SET ROLE billing_service;
DO $$
DECLARE is_superuser BOOLEAN;
BEGIN
    SELECT rolsuper INTO is_superuser FROM pg_catalog.pg_roles WHERE rolname=session_user;
    IF session_user<>'billing_migrator' AND NOT is_superuser THEN
        RAISE EXCEPTION '000075 requires the dedicated billing_migrator deployment identity';
    END IF;
    IF NOT is_superuser AND current_user<>'billing_service' THEN
        RAISE EXCEPTION '000075 must run with MIGRATION_ROLE=billing_service';
    END IF;
    IF NOT pg_catalog.pg_has_role(session_user,'billing_schema_owner','MEMBER')
       OR pg_catalog.pg_has_role('billing_service','billing_schema_owner','MEMBER') THEN
        RAISE EXCEPTION '000075 schema owner must remain separate from runtime';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_class
                   WHERE relname='usage_settlement_repair_cases' AND relkind='r') THEN
        RAISE EXCEPTION '000075 requires the usage settlement repair case table from 000074';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint
                   WHERE conname='usage_loss_reviews_status_check'
                     AND conrelid='public.usage_loss_reviews'::regclass
                     AND pg_catalog.pg_get_constraintdef(oid) LIKE '%RESOLVED_PLATFORM_LOSS%') THEN
        RAISE EXCEPTION '000075 requires the existing usage loss review states from 000016';
    END IF;
END $$;

SET ROLE billing_service;
-- GSA-36 introduces exactly one new state edge: a loss review that the governed
-- repair has settled against real entitlement capacity becomes RESOLVED_SETTLED.
-- The existing states keep their meaning and the existing writers keep working.
-- The review table is runtime-owned (000016 created it as billing_service), so
-- the runtime owner performs the in-place constraint swap; the schema owner
-- stays separate and no ownership is handed over.
ALTER TABLE public.usage_loss_reviews DROP CONSTRAINT usage_loss_reviews_status_check;
ALTER TABLE public.usage_loss_reviews
    ADD CONSTRAINT usage_loss_reviews_status_check
    CHECK (status IN ('REVIEW_REQUIRED','RESOLVED_PLATFORM_LOSS','RESOLVED_SETTLED'));
SET ROLE billing_service;
