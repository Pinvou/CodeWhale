-- Preserve original 000020 bytes while retaining durable membership snapshots.
SET ROLE billing_service;
DO $$
BEGIN
    IF session_user <> 'billing_migrator'
       OR current_user <> 'billing_service'
       OR NOT pg_catalog.pg_has_role(session_user,'billing_schema_owner','MEMBER')
       OR pg_catalog.pg_has_role('billing_service','billing_schema_owner','MEMBER') THEN
        RAISE EXCEPTION 'member lifecycle event identity requires the dedicated deployment roles';
    END IF;
    IF (SELECT pg_catalog.pg_get_userbyid(relowner) FROM pg_catalog.pg_class
        WHERE oid='public.organization_member_lifecycle_states'::regclass) <> 'billing_service' THEN
        RAISE EXCEPTION 'member lifecycle state must remain owned by billing_service';
    END IF;
END $$;

LOCK TABLE public.organization_member_lifecycle_states IN ACCESS EXCLUSIVE MODE;
ALTER TABLE public.organization_member_lifecycle_states
    ADD COLUMN membership_aggregate_id UUID UNIQUE;
ALTER TABLE public.organization_member_lifecycle_states
    DROP CONSTRAINT organization_member_lifecycle_states_status_check;
ALTER TABLE public.organization_member_lifecycle_states
    ADD CONSTRAINT organization_member_lifecycle_states_status_check
        CHECK (status IN ('ACTIVE','SUSPENDED','REVOKED'));
