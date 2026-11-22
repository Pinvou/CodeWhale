SET ROLE billing_service;
DO $$
DECLARE is_superuser BOOLEAN;
BEGIN
    SELECT rolsuper INTO is_superuser FROM pg_catalog.pg_roles WHERE rolname=session_user;
    IF session_user<>'billing_migrator' AND NOT is_superuser THEN
        RAISE EXCEPTION '000058 requires the dedicated billing_migrator deployment identity';
    END IF;
    IF NOT is_superuser AND current_user<>'billing_service' THEN
        RAISE EXCEPTION '000058 must run with MIGRATION_ROLE=billing_service';
    END IF;
    IF NOT pg_catalog.pg_has_role(session_user,'billing_schema_owner','MEMBER')
       OR pg_catalog.pg_has_role('billing_service','billing_schema_owner','MEMBER') THEN
        RAISE EXCEPTION '000058 schema owner must remain separate from runtime';
    END IF;
    IF (SELECT pg_catalog.pg_get_userbyid(relowner) FROM pg_catalog.pg_class
        WHERE oid='public.entitlement_components'::regclass) <> 'billing_service'
       OR (SELECT pg_catalog.pg_get_userbyid(relowner) FROM pg_catalog.pg_class
        WHERE oid='public.entitlement_component_corrections'::regclass) <> 'billing_schema_owner' THEN
        RAISE EXCEPTION '000058 correction effect table ownership is invalid';
    END IF;
    IF EXISTS (SELECT 1 FROM public.entitlement_components WHERE reclassified_out_microcredits <> 0) THEN
        RAISE EXCEPTION '000058 requires every reclassified amount to still be zero';
    END IF;
END $$;

SET ROLE billing_service;
-- The correction effect adds one target component/lot per corrected package, so
-- the one-component-per-type uniqueness and the zero-only transfer gate are
-- replaced by the effective conservation the domain now maintains.
ALTER TABLE public.entitlement_components DROP CONSTRAINT entitlement_components_package_id_component_type_key;
DO $$
DECLARE old_conservation TEXT;
BEGIN
    SELECT c.conname INTO old_conservation
    FROM pg_catalog.pg_constraint c
    WHERE c.conrelid='public.entitlement_components'::regclass
      AND c.contype='c'
      AND pg_catalog.pg_get_constraintdef(c.oid) LIKE '%available_microcredits%'
      AND pg_catalog.pg_get_constraintdef(c.oid) LIKE '%original_microcredits%'
      AND pg_catalog.pg_get_constraintdef(c.oid) NOT LIKE '%reclassified_out_microcredits%';
    IF old_conservation IS NULL THEN
        RAISE EXCEPTION '000058 cannot identify the legacy entitlement conservation constraint';
    END IF;
    EXECUTE 'ALTER TABLE public.entitlement_components DROP CONSTRAINT ' || quote_ident(old_conservation);
END $$;
ALTER TABLE public.entitlement_components DROP CONSTRAINT entitlement_components_correction_inactive;
ALTER TABLE public.entitlement_components ADD CONSTRAINT entitlement_components_effective_conservation_check CHECK (
    available_microcredits + reserved_microcredits + used_microcredits +
    active_grant_microcredits + suspended_grant_microcredits +
    expired_microcredits + revoked_microcredits + reclassified_out_microcredits = original_microcredits
);

DROP TRIGGER entitlement_components_project_package ON public.entitlement_components;
SET ROLE billing_schema_owner;
CREATE FUNCTION public.project_entitlement_package_components_v2() RETURNS trigger AS $$
DECLARE target_package UUID;
BEGIN
    target_package := CASE WHEN TG_OP='DELETE' THEN OLD.package_id ELSE NEW.package_id END;
    UPDATE public.entitlement_packages package SET
        points_total = totals.effective_original,
        points_remaining = totals.available + totals.active_grant + totals.suspended_grant,
        points_reserved = totals.reserved,
        points_used = totals.used,
        points_expired = totals.expired,
        points_revoked = totals.revoked,
        updated_at = GREATEST(package.updated_at, totals.updated_at)
    FROM (
        SELECT COALESCE(sum(original_microcredits-reclassified_out_microcredits),0) effective_original,
               COALESCE(sum(available_microcredits),0) available,
               COALESCE(sum(reserved_microcredits),0) reserved,
               COALESCE(sum(used_microcredits),0) used,
               COALESCE(sum(active_grant_microcredits),0) active_grant,
               COALESCE(sum(suspended_grant_microcredits),0) suspended_grant,
               COALESCE(sum(expired_microcredits),0) expired,
               COALESCE(sum(revoked_microcredits),0) revoked,
               COALESCE(max(updated_at),CURRENT_TIMESTAMP) updated_at
        FROM public.entitlement_components WHERE package_id=target_package
    ) totals
    WHERE package.id=target_package AND totals.effective_original>0;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql SET search_path=pg_catalog;
SET ROLE billing_service;
CREATE TRIGGER entitlement_components_project_package
AFTER INSERT OR UPDATE OR DELETE ON public.entitlement_components
FOR EACH ROW EXECUTE FUNCTION public.project_entitlement_package_components_v2();

SET ROLE billing_schema_owner;
-- The proof/activation gates remain closed, but the committed domain effect
-- needs append-only access to its own immutable case table.
GRANT INSERT ON public.entitlement_component_corrections TO billing_service;
SET ROLE billing_service;
