SET ROLE billing_service;
DO $$
DECLARE is_superuser BOOLEAN;
BEGIN
    SELECT rolsuper INTO is_superuser FROM pg_catalog.pg_roles WHERE rolname=session_user;
    IF session_user<>'billing_migrator' AND NOT is_superuser THEN
        RAISE EXCEPTION '000069 requires the dedicated billing_migrator deployment identity';
    END IF;
    IF NOT is_superuser AND current_user<>'billing_service' THEN
        RAISE EXCEPTION '000069 must run with MIGRATION_ROLE=billing_service';
    END IF;
    IF NOT pg_catalog.pg_has_role(session_user,'billing_schema_owner','MEMBER')
       OR pg_catalog.pg_has_role('billing_service','billing_schema_owner','MEMBER') THEN
        RAISE EXCEPTION '000069 schema owner must remain separate from runtime';
    END IF;
    -- The superseded composition check from 000011 is still present because
    -- 000016 dropped the wrong constraint name; it makes EXPIRED/REVOKED package
    -- states unrepresentable and therefore breaks every path that closes a
    -- package (reviewed refunds, forced refunds, chargebacks, expiries).
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint
                   WHERE conname='entitlement_packages_check1'
                     AND conrelid='public.entitlement_packages'::regclass
                     AND pg_catalog.pg_get_constraintdef(oid) LIKE '%points_reserved + points_used%'
                     AND pg_catalog.pg_get_constraintdef(oid) NOT LIKE '%points_revoked%') THEN
        RAISE EXCEPTION '000069 requires the superseded composition check from 000011';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint
                   WHERE conname='entitlement_packages_composition_check'
                     AND conrelid='public.entitlement_packages'::regclass
                     AND pg_catalog.pg_get_constraintdef(oid) LIKE '%points_revoked%') THEN
        RAISE EXCEPTION '000069 requires the current composition check from 000016';
    END IF;
END $$;

-- Forward-only cleanup: drop the superseded check. No row, column or history is
-- rewritten, and the current composition check (including points_expired and
-- points_revoked) stays in force. The package table is owned by the runtime, so
-- the drop runs as the owner exactly like the constraint rewrites in 000060.
SET ROLE billing_service;
ALTER TABLE public.entitlement_packages DROP CONSTRAINT entitlement_packages_check1;
SET ROLE billing_service;
