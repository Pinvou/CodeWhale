DO $$ BEGIN
  IF session_user<>'account_migrator' OR current_user<>'account_service' THEN
    RAISE EXCEPTION '000052 down requires account_migrator with MIGRATION_ROLE account_service' USING ERRCODE='42501';
  END IF;
END $$;

RESET ROLE;
SET ROLE account_approval_control_owner;
DO $$ DECLARE control_row account_approval_control.admin_approval_control%ROWTYPE;
BEGIN
  SELECT * INTO STRICT control_row FROM account_approval_control.admin_approval_control
   WHERE control_id='ADMIN_APPROVAL' FOR UPDATE;
  IF control_row.mode<>'LEGACY' OR control_row.authorization_relay_state<>'RUNNING'
     OR control_row.approval_epoch<>1 OR control_row.authorization_relay_epoch<>1 THEN
    RAISE EXCEPTION 'approval relay control is not safe for rollback' USING ERRCODE='55000';
  END IF;
END $$;

RESET ROLE;
SET ROLE account_approval_authorization_owner;
LOCK TABLE public.outbox_events IN ACCESS EXCLUSIVE MODE;
LOCK TABLE public.event_replay_requests IN ACCESS EXCLUSIVE MODE;
DO $$ BEGIN
  IF EXISTS(SELECT 1 FROM public.outbox_events WHERE relay_claim_token IS NOT NULL OR relay_control_epoch IS NOT NULL OR relay_claim_worker IS NOT NULL)
     OR EXISTS(SELECT 1 FROM account_approval_authorization.authorization_delivery_pending) THEN
    RAISE EXCEPTION 'approval relay claim history must be preserved' USING ERRCODE='55000';
  END IF;
END $$;

DROP TRIGGER event_replay_authorization_guard ON public.event_replay_requests;
DROP FUNCTION public.inspect_authorization_dead_letter(UUID);
DROP TRIGGER approval_authorization_delivery_pending_guard ON account_approval_authorization.authorization_delivery_pending;
DROP FUNCTION account_approval_authorization.guard_authorization_replay_insert();
DROP FUNCTION account_approval_authorization.authorization_relay_readiness();
DROP FUNCTION account_approval_authorization.authorization_relay_outcome(UUID,UUID,BIGINT,TEXT);
DROP FUNCTION account_approval_authorization.finish_authorization_delivery(UUID,UUID,BIGINT,TEXT,TEXT,TIMESTAMPTZ,TEXT);
DROP FUNCTION account_approval_authorization.open_authorization_delivery(UUID,UUID,BIGINT,TEXT);
DROP FUNCTION account_approval_authorization.claim_authorization_events(TEXT,INTEGER,INTEGER);
DROP FUNCTION account_approval_authorization.authorization_relay_mode_allows(TEXT,TEXT,BIGINT,BIGINT,BIGINT);
DROP FUNCTION account_approval_authorization.reject_unfinished_delivery();
DROP TABLE account_approval_authorization.authorization_delivery_pending;
DROP INDEX public.outbox_events_authorization_relay_claim_token_uidx;
DROP INDEX public.outbox_events_authorization_relay_idx;
REVOKE USAGE ON SCHEMA account_approval_authorization FROM account_approval_relay;

CREATE OR REPLACE FUNCTION account_approval_authorization.guard_outbox_update() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
    IF OLD.event_type='account.approval-command.authorized.v1' OR NEW.event_type='account.approval-command.authorized.v1' THEN
        IF NEW.event_id IS DISTINCT FROM OLD.event_id OR NEW.event_type IS DISTINCT FROM OLD.event_type
           OR NEW.aggregate_id IS DISTINCT FROM OLD.aggregate_id OR NEW.aggregate_version IS DISTINCT FROM OLD.aggregate_version
           OR NEW.payload IS DISTINCT FROM OLD.payload OR NEW.trace_id IS DISTINCT FROM OLD.trace_id
           OR NEW.occurred_at IS DISTINCT FROM OLD.occurred_at THEN
            RAISE EXCEPTION 'authorization outbox identity and payload are immutable' USING ERRCODE='55000';
        END IF;
        IF NEW.approval_control_epoch IS DISTINCT FROM OLD.approval_control_epoch
           AND NOT (session_user='account_approval_backfill' AND OLD.approval_control_epoch IS NULL AND NEW.approval_control_epoch=1) THEN
            RAISE EXCEPTION 'authorization outbox epoch is immutable' USING ERRCODE='55000';
        END IF;
    ELSIF NEW.approval_control_epoch IS NOT NULL THEN
        RAISE EXCEPTION 'non-authorization outbox event cannot bind approval epoch' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION account_approval_authorization.validate_control_transition_sidecars() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE summary RECORD;
BEGIN
    IF OLD.mode='LEGACY' AND NEW.mode='QUIESCING' THEN
        SELECT * INTO summary FROM account_approval_authorization.authorization_sidecar_summary();
        IF summary.missing_epoch_count<>0 OR summary.missing_sidecar_count<>0 OR summary.invalid_sidecar_count<>0 THEN
            RAISE EXCEPTION 'authorization outbox is not ready for QUIESCING' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NULL;
END $$;

ALTER TABLE public.outbox_events DROP CONSTRAINT outbox_authorization_relay_pair;
ALTER TABLE public.outbox_events DROP CONSTRAINT outbox_authorization_delivery_shape;
ALTER TABLE public.outbox_events DROP COLUMN relay_control_epoch,DROP COLUMN relay_claim_token,DROP COLUMN relay_claim_worker;

RESET ROLE;
GRANT account_approval_authorization_owner TO account_service WITH INHERIT TRUE, SET TRUE;
SET ROLE account_service;
ALTER TABLE public.event_replay_requests OWNER TO account_service;
ALTER FUNCTION public.reject_event_replay_audit_mutation() OWNER TO account_service;

RESET ROLE;
REVOKE account_approval_authorization_owner FROM account_service;
SET ROLE account_approval_authorization_owner;
DO $$ BEGIN
  IF pg_catalog.to_regprocedure('account_approval_authorization.claim_authorization_events(text,integer,integer)') IS NOT NULL THEN
    RAISE EXCEPTION '000052 down protected function cleanup failed';
  END IF;
END $$;

RESET ROLE;
SET ROLE account_service;
DO $$ BEGIN
  IF pg_catalog.pg_get_userbyid((SELECT relowner FROM pg_catalog.pg_class WHERE oid='public.event_replay_requests'::regclass))<>'account_service'
	 OR pg_catalog.pg_get_userbyid((SELECT proowner FROM pg_catalog.pg_proc WHERE oid='public.reject_event_replay_audit_mutation()'::regprocedure))<>'account_service'
     OR EXISTS(SELECT 1 FROM pg_catalog.pg_attribute WHERE attrelid='public.outbox_events'::regclass
          AND attname IN ('relay_control_epoch','relay_claim_token','relay_claim_worker') AND NOT attisdropped)
     OR pg_catalog.to_regclass('public.outbox_events_authorization_relay_idx') IS NOT NULL
	 OR pg_catalog.to_regclass('public.outbox_events_authorization_relay_claim_token_uidx') IS NOT NULL
	 OR pg_catalog.to_regprocedure('public.inspect_authorization_dead_letter(uuid)') IS NOT NULL
	 OR EXISTS(SELECT 1 FROM pg_catalog.pg_trigger WHERE tgrelid='public.event_replay_requests'::regclass AND tgname='event_replay_authorization_guard')
	 OR pg_catalog.pg_has_role('account_service','account_approval_authorization_owner','MEMBER')
	 OR pg_catalog.has_schema_privilege('account_approval_relay','account_approval_authorization','USAGE')
	 OR pg_catalog.has_database_privilege('account_approval_relay',current_database(),'CREATE')
	 OR EXISTS(SELECT 1 FROM pg_catalog.pg_auth_members WHERE roleid='account_approval_relay'::regrole OR member='account_approval_relay'::regrole)
	 OR EXISTS(SELECT 1 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname IN ('public','account_approval_authorization','account_approval_control') AND c.relkind IN ('r','p','v','m','f')
		AND (pg_catalog.has_any_column_privilege('account_approval_relay',c.oid,'SELECT,INSERT,UPDATE,REFERENCES')
		 OR pg_catalog.has_table_privilege('account_approval_relay',c.oid,'DELETE,TRUNCATE,TRIGGER'))) THEN
    RAISE EXCEPTION '000052 down cleanup failed';
  END IF;
END $$;
