DO $$
BEGIN
    IF session_user<>'token_migrator' AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname=session_user AND rolsuper) THEN
        RAISE EXCEPTION '000034 down requires the dedicated token_migrator identity';
    END IF;
END $$;
SET LOCAL lock_timeout = '2s';
-- The compatibility trigger belongs to the pre-existing runtime-owned table.
SET ROLE token_gateway;
DROP TRIGGER IF EXISTS usage_history_projection_after_insert ON public.usage_requests;
SET ROLE token_schema_owner;
DROP FUNCTION IF EXISTS public.backfill_usage_history_projection(INTEGER);
DROP FUNCTION IF EXISTS public.maintain_usage_history_projection();
DROP FUNCTION IF EXISTS public.project_usage_history_request(UUID,UUID,UUID,JSONB,TIMESTAMPTZ);
DROP TABLE IF EXISTS public.usage_history_groups;
DROP TABLE IF EXISTS public.usage_history_members;
DROP TABLE IF EXISTS public.usage_history_projection_state;
SET ROLE token_gateway;
REVOKE SELECT,REFERENCES ON public.usage_requests FROM token_schema_owner;
