-- Additive authorization fence. 000051 retains its immutable migration bytes.
DO $$
BEGIN
  IF session_user<>'account_migrator' OR current_user<>'account_service' THEN
    RAISE EXCEPTION '900010 requires isolated account migration identities' USING ERRCODE='42501';
  END IF;
  IF NOT EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=51) THEN
    RAISE EXCEPTION '900010 requires the original approval authorization boundary' USING ERRCODE='55000';
  END IF;
END $$;

-- The migrator already has SET-only membership in this NOLOGIN owner. The
-- service never receives membership, function ownership or runtime table ACLs.
GRANT CREATE ON SCHEMA public TO account_approval_authorization_owner;
SET ROLE account_approval_authorization_owner;
CREATE OR REPLACE FUNCTION public.lock_approval_writer_authorization(p_user_id UUID,p_permission TEXT)
RETURNS TABLE(binding_id UUID,authorization_version BIGINT,valid_until TIMESTAMPTZ)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE
    locked_bindings UUID[];
    v_now TIMESTAMPTZ;
BEGIN
    IF session_user<>'account_approval_writer' THEN
        RAISE EXCEPTION 'approval writer authorization identity is invalid' USING ERRCODE='42501';
    END IF;
    PERFORM u.id FROM public.users u WHERE u.id=p_user_id FOR SHARE;
    IF NOT FOUND THEN RETURN; END IF;
    SELECT COALESCE(pg_catalog.array_agg(locked.id ORDER BY locked.id),'{}'::UUID[])
      INTO locked_bindings FROM (
        SELECT ra.id FROM public.role_assignments ra
        JOIN public.role_permissions rp ON rp.role_code=ra.role_code AND rp.permission_code=p_permission
        WHERE ra.user_id=p_user_id AND ra.organization_id IS NULL AND ra.revoked_at IS NULL
        ORDER BY ra.id FOR SHARE OF ra
      ) locked;
    SELECT av.version INTO authorization_version FROM public.authorization_versions av
     WHERE av.user_id=p_user_id AND av.scope='PLATFORM'
       AND av.scope_id='00000000-0000-0000-0000-000000000000'::UUID FOR SHARE;
    IF NOT FOUND THEN authorization_version := 1; END IF;
    v_now := pg_catalog.clock_timestamp();
    SELECT ra.id,ra.expires_at INTO binding_id,valid_until
      FROM public.users u
      JOIN public.role_assignments ra ON ra.user_id=u.id AND ra.organization_id IS NULL
      JOIN public.role_permissions rp ON rp.role_code=ra.role_code AND rp.permission_code=p_permission
     WHERE u.id=p_user_id AND u.status='ACTIVE' AND ra.id=ANY(locked_bindings)
       AND ra.revoked_at IS NULL AND ra.starts_at<=v_now
       AND (ra.expires_at IS NULL OR ra.expires_at>v_now)
     ORDER BY ra.expires_at NULLS LAST,ra.id LIMIT 1;
    IF NOT FOUND THEN RETURN; END IF;
    RETURN NEXT;
END $$;
RESET ROLE;
SET ROLE account_service;
REVOKE CREATE ON SCHEMA public FROM account_approval_authorization_owner;
