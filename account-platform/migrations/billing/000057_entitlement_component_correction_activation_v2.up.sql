SET ROLE billing_service;
DO $$
DECLARE is_superuser BOOLEAN;
BEGIN
    SELECT rolsuper INTO is_superuser FROM pg_catalog.pg_roles WHERE rolname=session_user;
    IF session_user<>'billing_migrator' AND NOT is_superuser THEN
        RAISE EXCEPTION '000057 requires the dedicated billing_migrator deployment identity';
    END IF;
    IF NOT is_superuser AND current_user<>'billing_service' THEN
        RAISE EXCEPTION '000057 must run with MIGRATION_ROLE=billing_service';
    END IF;
    IF NOT pg_catalog.pg_has_role(session_user,'billing_schema_owner','MEMBER')
       OR pg_catalog.pg_has_role('billing_service','billing_schema_owner','MEMBER') THEN
        RAISE EXCEPTION '000057 schema owner must remain separate from runtime';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM public.billing_approval_command_activations
                   WHERE command_type='ENTITLEMENT_COMPONENT_ADJUSTMENT' AND policy_version=1
                     AND snapshot_schema_version=1 AND state='INACTIVE') THEN
        RAISE EXCEPTION '000057 requires the immutable inactive v1 entitlement activation';
    END IF;
    IF EXISTS (SELECT 1 FROM public.billing_approval_command_activations
               WHERE command_type='ENTITLEMENT_COMPONENT_ADJUSTMENT' AND policy_version<>1) THEN
        RAISE EXCEPTION '000057 entitlement activation v2 already exists';
    END IF;
END $$;

SET ROLE billing_schema_owner;
-- Append-only: the v1 row stays immutable history and the new row is the
-- highest known version for this single command. It must remain INACTIVE
-- until the complete GSA-22 domain gate set exists and activation is
-- separately authorized, so this migration opens nothing by itself.
INSERT INTO public.billing_approval_command_activations
    (command_type,policy_version,snapshot_schema_version,state,version,updated_at)
VALUES
    ('ENTITLEMENT_COMPONENT_ADJUSTMENT',2,2,'INACTIVE',1,transaction_timestamp());
SET ROLE billing_service;
