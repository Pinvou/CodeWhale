-- The runner executes this entire rollback in one transaction. Preserve all
-- authorization history and follow the writer's control -> outbox lock order.
DO $$ BEGIN
    IF session_user <> 'account_migrator' THEN
        RAISE EXCEPTION '000051 down requires account_migrator' USING ERRCODE='42501';
    END IF;
    IF pg_catalog.pg_has_role('account_service','account_approval_authorization_owner','MEMBER') THEN
        RAISE EXCEPTION 'unexpected authorization owner membership' USING ERRCODE='42501';
    END IF;
END $$;
RESET ROLE;
SET ROLE account_approval_control_owner;
DO $$
DECLARE control_row account_approval_control.admin_approval_control%ROWTYPE;
BEGIN
    SELECT * INTO STRICT control_row FROM account_approval_control.admin_approval_control
      WHERE control_id='ADMIN_APPROVAL' FOR UPDATE;
    IF control_row.schema_version<>1 OR control_row.mode<>'LEGACY'
       OR control_row.authorization_relay_state<>'RUNNING'
       OR control_row.approval_epoch<>1 OR control_row.approval_wire_version<>1 OR control_row.proof_required THEN
        RAISE EXCEPTION 'approval authorization control is not safe for rollback' USING ERRCODE='55000';
    END IF;
END $$;
RESET ROLE;
SET ROLE account_approval_authorization_owner;
LOCK TABLE public.outbox_events IN ACCESS EXCLUSIVE MODE;
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM public.outbox_events WHERE approval_control_epoch IS NOT NULL)
       OR EXISTS (SELECT 1 FROM account_approval_authorization.approval_authorization_event_bytes)
       OR EXISTS (SELECT 1 FROM account_approval_authorization.authorization_event_pending) THEN
        RAISE EXCEPTION 'approval authorization history must be preserved' USING ERRCODE='55000';
    END IF;
END $$;

RESET ROLE;
SET ROLE account_approval_control_owner;
DROP TRIGGER approval_authorization_control_transition_guard ON account_approval_control.admin_approval_control;
REVOKE EXECUTE ON FUNCTION account_approval_control.lock_admin_approval_control_shared()
    FROM account_approval_authorization_owner,account_approval_writer;
DROP FUNCTION account_approval_control.lock_admin_approval_control_exclusive_for_authorization();
REVOKE USAGE ON SCHEMA account_approval_control
    FROM account_approval_authorization_owner,account_approval_writer;

RESET ROLE;
SET ROLE account_service;
DROP TRIGGER approval_authorization_request_transition_guard ON public.approval_requests;
REVOKE ALL ON public.identities,public.users,public.role_assignments,public.role_permissions,
    public.authorization_versions,public.approval_command_policies,public.approval_requests,
    public.approval_request_snapshots,public.approval_decisions,public.approval_state_events
    FROM account_approval_writer,account_approval_authorization_owner;
-- Table-level REVOKE does not remove column-level ACLs.
REVOKE SELECT(id,status,version,command_type,policy_version,target_resource_type,target_resource_id),UPDATE(version)
    ON public.approval_requests FROM account_approval_authorization_owner;
REVOKE SELECT(id,approval_request_id,decision) ON public.approval_decisions FROM account_approval_authorization_owner;
REVOKE SELECT(id,approval_request_id,to_status,reason_code) ON public.approval_state_events FROM account_approval_authorization_owner;
REVOKE UPDATE(id) ON public.users,public.role_assignments FROM account_approval_authorization_owner;
REVOKE UPDATE(version) ON public.authorization_versions FROM account_approval_authorization_owner;
REVOKE UPDATE(status,execution_deadline,decided_at,invalidation_code,invalidation_details,version,updated_at)
    ON public.approval_requests FROM account_approval_writer;

RESET ROLE;
SET ROLE account_approval_authorization_owner;
DROP FUNCTION public.lock_approval_writer_authorization(UUID,TEXT);
DROP INDEX public.outbox_events_authorization_backfill_idx;
DROP INDEX public.outbox_events_authorization_version_uidx;
DROP SCHEMA account_approval_authorization CASCADE;

RESET ROLE;
-- This temporary inherited membership is transactional. NOINHERIT membership
-- alone cannot ALTER the old owner's table; no service ADMIN grant is needed.
GRANT account_approval_authorization_owner TO account_service WITH INHERIT TRUE, SET TRUE;
SET ROLE account_service;
ALTER TABLE public.outbox_events DROP COLUMN approval_control_epoch;
ALTER TABLE public.outbox_events OWNER TO account_service;
REVOKE USAGE ON SCHEMA public FROM account_approval_authorization_owner;

RESET ROLE;
REVOKE account_approval_authorization_owner FROM account_service;
DO $$ BEGIN
    IF pg_catalog.pg_get_userbyid((SELECT relowner FROM pg_catalog.pg_class WHERE oid='public.outbox_events'::regclass))<>'account_service'
       OR pg_catalog.to_regnamespace('account_approval_authorization') IS NOT NULL
       OR pg_catalog.to_regprocedure('public.lock_approval_writer_authorization(uuid,text)') IS NOT NULL
       OR EXISTS (SELECT 1 FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
           WHERE n.nspname='account_approval_control' AND p.proname='lock_admin_approval_control_exclusive_for_authorization')
       OR pg_catalog.to_regclass('public.outbox_events_authorization_backfill_idx') IS NOT NULL
       OR pg_catalog.to_regclass('public.outbox_events_authorization_version_uidx') IS NOT NULL
       OR EXISTS (SELECT 1 FROM pg_catalog.pg_trigger WHERE tgrelid='public.approval_requests'::regclass
           AND tgname='approval_authorization_request_transition_guard')
       OR pg_catalog.pg_has_role('account_service','account_approval_authorization_owner','MEMBER')
       OR pg_catalog.has_database_privilege('account_migrator',current_database(),'CREATE')
       OR EXISTS (
           SELECT 1 FROM pg_catalog.pg_attribute a WHERE a.attrelid='public.outbox_events'::regclass
             AND a.attname='approval_control_epoch' AND NOT a.attisdropped
       ) OR EXISTS (
           SELECT 1 FROM pg_catalog.pg_class c, LATERAL pg_catalog.aclexplode(c.relacl) acl
            WHERE c.relnamespace='public'::regnamespace
              AND acl.grantee IN ('account_approval_writer'::regrole,'account_approval_backfill'::regrole,'account_approval_authorization_owner'::regrole)
       ) OR EXISTS (
           SELECT 1 FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_class c ON c.oid=a.attrelid,
             LATERAL pg_catalog.aclexplode(a.attacl) acl
            WHERE c.relnamespace='public'::regnamespace
              AND acl.grantee IN ('account_approval_writer'::regrole,'account_approval_backfill'::regrole,'account_approval_authorization_owner'::regrole)
       ) THEN
        RAISE EXCEPTION '000051 down owner or privilege cleanup failed';
    END IF;
END $$;
SET ROLE account_service;
