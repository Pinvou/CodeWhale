-- GSA-07: fenced relay for the authorization subject. This migration is
-- intentionally schema-first and may only be installed at the original
-- LEGACY/RUNNING epoch tuple.
DO $$
DECLARE
    owner_oid OID := (SELECT oid FROM pg_catalog.pg_roles WHERE rolname='account_approval_authorization_owner');
    migrator_oid OID := (SELECT oid FROM pg_catalog.pg_roles WHERE rolname='account_migrator');
    relay_oid OID := (SELECT oid FROM pg_catalog.pg_roles WHERE rolname='account_approval_relay');
BEGIN
    IF session_user<>'account_migrator' OR current_user<>'account_service' THEN
        RAISE EXCEPTION '000052 requires account_migrator with MIGRATION_ROLE account_service';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE oid=relay_oid AND rolcanlogin AND NOT rolinherit
        AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole AND NOT rolreplication AND NOT rolbypassrls) THEN
        RAISE EXCEPTION '000052 requires isolated account_approval_relay role';
    END IF;
    IF pg_catalog.pg_get_userbyid((SELECT relowner FROM pg_catalog.pg_class WHERE oid='public.outbox_events'::regclass))<>'account_approval_authorization_owner'
       OR owner_oid IS NULL OR migrator_oid IS NULL
       OR EXISTS (SELECT 1 FROM pg_catalog.pg_auth_members am
          WHERE (am.roleid=relay_oid OR am.member=relay_oid)
             OR ((am.roleid=owner_oid OR am.member=owner_oid)
                 AND NOT (am.roleid=owner_oid AND am.member=migrator_oid AND am.admin_option AND am.set_option AND NOT am.inherit_option))) THEN
        RAISE EXCEPTION '000052 role or owner preflight failed';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM account_approval_control.admin_approval_control
        WHERE control_id='ADMIN_APPROVAL' AND mode='LEGACY' AND authorization_relay_state='RUNNING'
          AND approval_epoch=1 AND authorization_relay_epoch=1)
       OR EXISTS (SELECT 1 FROM public.outbox_events
          WHERE event_type='account.approval-command.authorized.v1'
            AND (approval_control_epoch IS DISTINCT FROM 1 OR locked_by IS NOT NULL OR locked_until IS NOT NULL)) THEN
        RAISE EXCEPTION '000052 requires the initial drained LEGACY/RUNNING epoch tuple' USING ERRCODE='55000';
    END IF;
END $$;

-- Move replay audit ownership out of the legacy runtime identity. The same
-- bounded membership pattern was established by 000051 and is removed in
-- this transaction before completion.
RESET ROLE;
GRANT account_approval_authorization_owner TO account_service WITH INHERIT FALSE, SET TRUE;
SET ROLE account_service;
GRANT CREATE ON SCHEMA public TO account_approval_authorization_owner;
ALTER TABLE public.event_replay_requests OWNER TO account_approval_authorization_owner;
ALTER FUNCTION public.reject_event_replay_audit_mutation() OWNER TO account_approval_authorization_owner;
RESET ROLE;
REVOKE account_approval_authorization_owner FROM account_service;

SET ROLE account_approval_authorization_owner;

DO $$ DECLARE readiness RECORD;
BEGIN
	SELECT * INTO STRICT readiness FROM account_approval_authorization.authorization_sidecar_summary();
	IF readiness.missing_epoch_count<>0 OR readiness.missing_sidecar_count<>0 OR readiness.invalid_sidecar_count<>0 THEN
		RAISE EXCEPTION '000052 requires a complete deliverable authorization sidecar set' USING ERRCODE='55000';
	END IF;
END $$;

-- Linearize Relay fencing after every claimant's shared control lock. A
-- committed claim (active or reclaimable) must be drained before FENCED can
-- commit; retry audit tokens without an active lease do not block fencing.
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
    IF OLD.authorization_relay_state='RUNNING' AND NEW.authorization_relay_state='FENCED'
       AND EXISTS (SELECT 1 FROM public.outbox_events
          WHERE event_type='account.approval-command.authorized.v1'
            AND published_at IS NULL AND dead_lettered_at IS NULL AND locked_by IS NOT NULL) THEN
        RAISE EXCEPTION 'authorization relay claims must be drained before fencing' USING ERRCODE='23514';
    END IF;
    RETURN NULL;
END $$;

ALTER TABLE public.outbox_events
    ADD COLUMN relay_control_epoch BIGINT,
    ADD COLUMN relay_claim_token UUID,
	ADD COLUMN relay_claim_worker TEXT,
    ADD CONSTRAINT outbox_authorization_relay_pair CHECK (
        (event_type='account.approval-command.authorized.v1' AND
            ((relay_control_epoch IS NULL AND relay_claim_token IS NULL AND relay_claim_worker IS NULL)
             OR (relay_control_epoch IS NOT NULL AND relay_control_epoch>0 AND relay_claim_token IS NOT NULL
			     AND relay_claim_worker IS NOT NULL AND char_length(relay_claim_worker) BETWEEN 1 AND 160 AND relay_claim_worker!~'[[:cntrl:]]')))
        OR (event_type<>'account.approval-command.authorized.v1' AND relay_control_epoch IS NULL AND relay_claim_token IS NULL AND relay_claim_worker IS NULL)
    ),
	ADD CONSTRAINT outbox_authorization_delivery_shape CHECK (
		event_type<>'account.approval-command.authorized.v1' OR (
			(locked_by IS NULL)=(locked_until IS NULL)
			AND NOT (published_at IS NOT NULL AND dead_lettered_at IS NOT NULL)
			AND ((published_at IS NULL AND dead_lettered_at IS NULL) OR (locked_by IS NULL AND locked_until IS NULL))
		)
	);

CREATE INDEX outbox_events_authorization_relay_idx
ON public.outbox_events(next_attempt_at,occurred_at,event_id)
WHERE event_type='account.approval-command.authorized.v1' AND published_at IS NULL AND dead_lettered_at IS NULL;

-- A claim token is a permanent audit identity, including after retry and
-- terminal close. Keep the collision boundary in PostgreSQL instead of
-- relying on the statistical properties of gen_random_uuid().
CREATE UNIQUE INDEX outbox_events_authorization_relay_claim_token_uidx
ON public.outbox_events(relay_claim_token)
WHERE relay_claim_token IS NOT NULL;

CREATE TABLE account_approval_authorization.authorization_delivery_pending (
    event_id UUID PRIMARY KEY,
    claim_token UUID NOT NULL,
    relay_control_epoch BIGINT NOT NULL CHECK(relay_control_epoch>0),
    worker_id TEXT NOT NULL CHECK(char_length(worker_id) BETWEEN 1 AND 160),
    created_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp()
);
REVOKE ALL ON account_approval_authorization.authorization_delivery_pending FROM PUBLIC,account_service,account_approval_writer,account_approval_backfill,account_approval_relay;

CREATE FUNCTION account_approval_authorization.reject_unfinished_delivery() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM account_approval_authorization.authorization_delivery_pending
       WHERE event_id=NEW.event_id AND claim_token=NEW.claim_token
         AND relay_control_epoch=NEW.relay_control_epoch AND worker_id=NEW.worker_id) THEN
        RAISE EXCEPTION 'authorization delivery was not finalized' USING ERRCODE='23514';
    END IF;
    RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER approval_authorization_delivery_pending_guard
AFTER INSERT ON account_approval_authorization.authorization_delivery_pending
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
EXECUTE FUNCTION account_approval_authorization.reject_unfinished_delivery();

CREATE FUNCTION account_approval_authorization.authorization_relay_mode_allows(
    p_mode TEXT,p_state TEXT,p_approval_epoch BIGINT,p_relay_epoch BIGINT,p_row_epoch BIGINT
) RETURNS BOOLEAN LANGUAGE sql STABLE SET search_path=pg_catalog AS $$
SELECT CASE
 WHEN p_state<>'RUNNING' OR p_relay_epoch<=0 THEN false
 WHEN p_mode IN ('LEGACY','QUIESCING','PROOF_REQUIRED') THEN p_row_epoch>0 AND p_row_epoch<=p_approval_epoch
 WHEN p_mode='SAFE_ROLLBACK' THEN p_row_epoch=p_approval_epoch-1
 ELSE false END
$$;
REVOKE ALL ON FUNCTION account_approval_authorization.authorization_relay_mode_allows(TEXT,TEXT,BIGINT,BIGINT,BIGINT) FROM PUBLIC,account_service,account_approval_writer,account_approval_backfill,account_approval_relay;

CREATE OR REPLACE FUNCTION account_approval_authorization.guard_outbox_update() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE
	control_row RECORD;
	now_value TIMESTAMPTZ := pg_catalog.clock_timestamp();
	is_claim BOOLEAN;
	is_publish BOOLEAN;
	is_retry BOOLEAN;
	is_dead BOOLEAN;
BEGIN
    IF OLD.event_type='account.approval-command.authorized.v1' OR NEW.event_type='account.approval-command.authorized.v1' THEN
        IF NEW.event_id IS DISTINCT FROM OLD.event_id OR NEW.event_type IS DISTINCT FROM OLD.event_type
           OR NEW.aggregate_id IS DISTINCT FROM OLD.aggregate_id OR NEW.aggregate_version IS DISTINCT FROM OLD.aggregate_version
           OR NEW.payload IS DISTINCT FROM OLD.payload OR NEW.trace_id IS DISTINCT FROM OLD.trace_id
           OR NEW.occurred_at IS DISTINCT FROM OLD.occurred_at OR NEW.approval_control_epoch IS DISTINCT FROM OLD.approval_control_epoch THEN
            RAISE EXCEPTION 'authorization outbox identity, payload, and approval epoch are immutable' USING ERRCODE='55000';
        END IF;
        IF (OLD.published_at IS NOT NULL AND NEW.published_at IS DISTINCT FROM OLD.published_at)
           OR (OLD.dead_lettered_at IS NOT NULL AND NEW.dead_lettered_at IS DISTINCT FROM OLD.dead_lettered_at)
           OR NEW.attempts<OLD.attempts OR NEW.published_at IS NOT NULL AND NEW.dead_lettered_at IS NOT NULL
           OR (NEW.locked_by IS NULL)<>(NEW.locked_until IS NULL)
           OR ((NEW.published_at IS NOT NULL OR NEW.dead_lettered_at IS NOT NULL) AND NEW.locked_by IS NOT NULL) THEN
            RAISE EXCEPTION 'authorization delivery terminal facts cannot be reverted' USING ERRCODE='55000';
        END IF;
		SELECT * INTO STRICT control_row FROM account_approval_control.lock_admin_approval_control_shared();
		is_claim := OLD.published_at IS NULL AND OLD.dead_lettered_at IS NULL
			AND (OLD.locked_until IS NULL OR OLD.locked_until<now_value)
			AND NEW.locked_by IS NOT NULL AND NEW.locked_by<>'' AND NEW.locked_by!~'[[:cntrl:]]'
			AND char_length(NEW.locked_by)<=160 AND NEW.locked_until>now_value AND NEW.locked_until<=now_value+interval '5 minutes'
			AND NEW.attempts=OLD.attempts AND NEW.next_attempt_at IS NOT DISTINCT FROM OLD.next_attempt_at
			AND NEW.last_error IS NOT DISTINCT FROM OLD.last_error AND NEW.published_at IS NOT DISTINCT FROM OLD.published_at
			AND NEW.dead_lettered_at IS NOT DISTINCT FROM OLD.dead_lettered_at;
		is_publish := OLD.locked_by IS NOT NULL AND OLD.published_at IS NULL AND OLD.dead_lettered_at IS NULL
			AND NEW.locked_by IS NULL AND NEW.locked_until IS NULL AND NEW.attempts=OLD.attempts+1
			AND NEW.published_at IS NOT NULL AND NEW.dead_lettered_at IS NULL AND NEW.last_error IS NULL
			AND NEW.next_attempt_at IS NOT DISTINCT FROM OLD.next_attempt_at;
		is_retry := OLD.locked_by IS NOT NULL AND OLD.published_at IS NULL AND OLD.dead_lettered_at IS NULL
			AND NEW.locked_by IS NULL AND NEW.locked_until IS NULL AND NEW.attempts=OLD.attempts+1 AND NEW.attempts<12
			AND NEW.published_at IS NULL AND NEW.dead_lettered_at IS NULL
			AND NEW.next_attempt_at>now_value AND NEW.next_attempt_at<=now_value+interval '10 minutes'
			AND NEW.last_error IN ('EVENT_DELIVERY_TIMEOUT','EVENT_BROKER_UNAVAILABLE','EVENT_DELIVERY_FAILED');
		is_dead := OLD.locked_by IS NOT NULL AND OLD.published_at IS NULL AND OLD.dead_lettered_at IS NULL
			AND NEW.locked_by IS NULL AND NEW.locked_until IS NULL AND NEW.attempts=OLD.attempts+1 AND NEW.attempts>=12
			AND NEW.published_at IS NULL AND NEW.dead_lettered_at IS NOT NULL
			AND NEW.next_attempt_at IS NOT DISTINCT FROM OLD.next_attempt_at
			AND NEW.last_error IN ('EVENT_DELIVERY_TIMEOUT','EVENT_BROKER_UNAVAILABLE','EVENT_DELIVERY_FAILED');
        IF session_user='account_service' THEN
            IF OLD.relay_control_epoch IS NOT NULL OR OLD.relay_claim_token IS NOT NULL OR OLD.relay_claim_worker IS NOT NULL
			   OR NEW.relay_control_epoch IS NOT NULL OR NEW.relay_claim_token IS NOT NULL OR NEW.relay_claim_worker IS NOT NULL THEN
                RAISE EXCEPTION 'legacy relay cannot write fenced claim facts' USING ERRCODE='55000';
            END IF;
			IF control_row.authorization_relay_epoch<>1 OR NOT (
				(control_row.mode='LEGACY' AND control_row.authorization_relay_state='RUNNING' AND control_row.approval_epoch=1 AND (is_claim OR is_publish OR is_retry OR is_dead))
				OR (control_row.mode='QUIESCING' AND control_row.authorization_relay_state='RUNNING' AND (is_publish OR is_retry OR is_dead))
			) THEN
                RAISE EXCEPTION 'legacy authorization relay shape is permanently closed' USING ERRCODE='55000';
            END IF;
		ELSIF session_user='account_approval_relay' THEN
			IF is_claim THEN
				IF NEW.relay_claim_token IS NULL OR NEW.relay_claim_worker IS DISTINCT FROM NEW.locked_by
				   OR NEW.relay_control_epoch IS DISTINCT FROM control_row.authorization_relay_epoch THEN
					RAISE EXCEPTION 'fenced authorization claim is invalid' USING ERRCODE='55000';
				END IF;
			ELSIF is_publish OR is_retry OR is_dead THEN
				IF NEW.relay_claim_token IS DISTINCT FROM OLD.relay_claim_token OR NEW.relay_claim_worker IS DISTINCT FROM OLD.relay_claim_worker
				   OR NEW.relay_claim_worker IS DISTINCT FROM OLD.locked_by OR NEW.relay_control_epoch IS DISTINCT FROM OLD.relay_control_epoch
				   OR NEW.relay_control_epoch IS DISTINCT FROM control_row.authorization_relay_epoch THEN
					RAISE EXCEPTION 'fenced authorization close is stale' USING ERRCODE='55000';
				END IF;
			ELSE
				RAISE EXCEPTION 'fenced authorization relay update shape is invalid' USING ERRCODE='55000';
			END IF;
		ELSE
            RAISE EXCEPTION 'authorization delivery mutation is not permitted' USING ERRCODE='55000';
        END IF;
    ELSIF NEW.approval_control_epoch IS NOT NULL OR NEW.relay_control_epoch IS NOT NULL OR NEW.relay_claim_token IS NOT NULL OR NEW.relay_claim_worker IS NOT NULL THEN
        RAISE EXCEPTION 'non-authorization outbox event cannot carry control facts' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END $$;

CREATE FUNCTION account_approval_authorization.claim_authorization_events(p_worker TEXT,p_limit INTEGER,p_lease_seconds INTEGER)
RETURNS TABLE(event_id UUID,event_type TEXT,trace_id TEXT,claim_token UUID,relay_control_epoch BIGINT)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE control_row RECORD;
BEGIN
    IF session_user<>'account_approval_relay' OR p_worker IS NULL OR p_worker<>btrim(p_worker)
       OR char_length(p_worker) NOT BETWEEN 1 AND 160 OR p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 1000
       OR p_lease_seconds IS NULL OR p_lease_seconds NOT BETWEEN 10 AND 300 THEN
        RAISE EXCEPTION 'authorization relay claim is invalid' USING ERRCODE='22023';
    END IF;
    SELECT * INTO STRICT control_row FROM account_approval_control.lock_admin_approval_control_shared();
    IF control_row.authorization_relay_state<>'RUNNING'
       OR control_row.mode NOT IN ('LEGACY','QUIESCING','PROOF_REQUIRED','SAFE_ROLLBACK') THEN
        RETURN;
    END IF;
    RETURN QUERY WITH candidates AS (
      SELECT o.event_id FROM public.outbox_events o
      JOIN account_approval_authorization.approval_authorization_event_bytes s ON s.event_id=o.event_id
      WHERE o.event_type='account.approval-command.authorized.v1' AND o.published_at IS NULL AND o.dead_lettered_at IS NULL
        AND o.next_attempt_at<=pg_catalog.clock_timestamp() AND (o.locked_until IS NULL OR o.locked_until<pg_catalog.clock_timestamp())
        AND s.provenance IN ('FROZEN_AT_APPROVAL','RECONSTRUCTED_FOR_DELIVERY') AND s.delivery_bytes IS NOT NULL AND s.event_digest IS NOT NULL
        AND account_approval_authorization.authorization_relay_mode_allows(control_row.mode,control_row.authorization_relay_state,
              control_row.approval_epoch,control_row.authorization_relay_epoch,o.approval_control_epoch)
      ORDER BY o.occurred_at,o.event_id FOR UPDATE OF o SKIP LOCKED LIMIT p_limit
    ), changed AS (
      UPDATE public.outbox_events o SET locked_by=p_worker,
        locked_until=pg_catalog.clock_timestamp()+pg_catalog.make_interval(secs=>p_lease_seconds),
        relay_claim_token=pg_catalog.gen_random_uuid(),relay_control_epoch=control_row.authorization_relay_epoch,relay_claim_worker=p_worker
      FROM candidates c WHERE o.event_id=c.event_id
      RETURNING o.event_id,o.event_type,o.trace_id,o.relay_claim_token,o.relay_control_epoch
    ) SELECT * FROM changed;
END $$;

CREATE FUNCTION account_approval_authorization.open_authorization_delivery(p_event UUID,p_token UUID,p_epoch BIGINT,p_worker TEXT)
RETURNS TABLE(event_type TEXT,trace_id TEXT,delivery_bytes BYTEA,event_digest TEXT,attempts INTEGER,locked_until TIMESTAMPTZ)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE control_row RECORD; outbox_row public.outbox_events%ROWTYPE; sidecar_row account_approval_authorization.approval_authorization_event_bytes%ROWTYPE;
BEGIN
    IF session_user<>'account_approval_relay' THEN RAISE EXCEPTION 'authorization relay identity required' USING ERRCODE='42501'; END IF;
    SELECT * INTO STRICT control_row FROM account_approval_control.lock_admin_approval_control_shared();
    SELECT * INTO STRICT outbox_row FROM public.outbox_events WHERE event_id=p_event FOR UPDATE;
    SELECT * INTO STRICT sidecar_row FROM account_approval_authorization.approval_authorization_event_bytes WHERE event_id=p_event;
    IF outbox_row.event_type<>'account.approval-command.authorized.v1' OR outbox_row.relay_claim_token IS DISTINCT FROM p_token
       OR outbox_row.relay_control_epoch IS DISTINCT FROM p_epoch OR outbox_row.locked_by IS DISTINCT FROM p_worker
	   OR outbox_row.relay_claim_worker IS DISTINCT FROM p_worker
	   OR p_epoch IS DISTINCT FROM control_row.authorization_relay_epoch
       OR outbox_row.locked_until IS NULL OR outbox_row.locked_until<=pg_catalog.clock_timestamp()
       OR outbox_row.published_at IS NOT NULL OR outbox_row.dead_lettered_at IS NOT NULL
       OR sidecar_row.provenance NOT IN ('FROZEN_AT_APPROVAL','RECONSTRUCTED_FOR_DELIVERY')
       OR sidecar_row.delivery_bytes IS NULL OR sidecar_row.event_digest IS NULL
       OR NOT account_approval_authorization.authorization_relay_mode_allows(control_row.mode,control_row.authorization_relay_state,
             control_row.approval_epoch,control_row.authorization_relay_epoch,outbox_row.approval_control_epoch)
       OR NOT account_approval_authorization.authorization_delivery_matches(outbox_row.event_id,outbox_row.event_type,outbox_row.aggregate_id,
             outbox_row.aggregate_version,outbox_row.payload,outbox_row.trace_id,outbox_row.occurred_at,outbox_row.approval_control_epoch,
             sidecar_row.serialization_version,sidecar_row.delivery_bytes) THEN
        RAISE EXCEPTION 'authorization relay claim is stale or invalid' USING ERRCODE='55000';
    END IF;
    INSERT INTO account_approval_authorization.authorization_delivery_pending(event_id,claim_token,relay_control_epoch,worker_id)
      VALUES(p_event,p_token,p_epoch,p_worker);
    RETURN QUERY SELECT outbox_row.event_type,outbox_row.trace_id,sidecar_row.delivery_bytes,sidecar_row.event_digest,outbox_row.attempts,outbox_row.locked_until;
END $$;

CREATE FUNCTION account_approval_authorization.finish_authorization_delivery(
    p_event UUID,p_token UUID,p_epoch BIGINT,p_worker TEXT,p_outcome TEXT,p_retry_at TIMESTAMPTZ,p_error_code TEXT
) RETURNS VOID LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE updated_count INTEGER;
BEGIN
    IF session_user<>'account_approval_relay' OR p_outcome IS NULL OR p_outcome NOT IN ('PUBLISHED','RETRY','DEAD_LETTERED')
	   OR (p_outcome='PUBLISHED' AND (p_retry_at IS NOT NULL OR p_error_code IS NOT NULL))
	   OR (p_outcome='RETRY' AND (p_retry_at IS NULL OR p_retry_at<=pg_catalog.clock_timestamp()
	       OR p_retry_at>pg_catalog.clock_timestamp()+interval '10 minutes'
	       OR p_error_code IS NULL OR p_error_code NOT IN ('EVENT_DELIVERY_TIMEOUT','EVENT_BROKER_UNAVAILABLE','EVENT_DELIVERY_FAILED')))
	   OR (p_outcome='DEAD_LETTERED' AND (p_retry_at IS NOT NULL OR p_error_code IS NULL
	       OR p_error_code NOT IN ('EVENT_DELIVERY_TIMEOUT','EVENT_BROKER_UNAVAILABLE','EVENT_DELIVERY_FAILED'))) THEN
        RAISE EXCEPTION 'authorization relay finish is invalid' USING ERRCODE='22023';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM account_approval_authorization.authorization_delivery_pending
        WHERE event_id=p_event AND claim_token=p_token AND relay_control_epoch=p_epoch AND worker_id=p_worker) THEN
        RAISE EXCEPTION 'authorization relay delivery was not opened' USING ERRCODE='55000';
    END IF;
    UPDATE public.outbox_events SET attempts=attempts+1,
      published_at=CASE WHEN p_outcome='PUBLISHED' THEN pg_catalog.clock_timestamp() ELSE published_at END,
      dead_lettered_at=CASE WHEN p_outcome='DEAD_LETTERED' THEN pg_catalog.clock_timestamp() ELSE dead_lettered_at END,
      next_attempt_at=CASE WHEN p_outcome='RETRY' THEN p_retry_at ELSE next_attempt_at END,
      last_error=CASE WHEN p_outcome='PUBLISHED' THEN NULL ELSE p_error_code END,
      locked_by=NULL,locked_until=NULL
    WHERE event_id=p_event AND relay_claim_token=p_token AND relay_control_epoch=p_epoch AND locked_by=p_worker
	  AND relay_claim_worker=p_worker
      AND published_at IS NULL AND dead_lettered_at IS NULL
	  AND ((p_outcome='RETRY' AND attempts+1<12) OR (p_outcome='DEAD_LETTERED' AND attempts+1>=12) OR p_outcome='PUBLISHED');
    GET DIAGNOSTICS updated_count=ROW_COUNT;
    IF updated_count<>1 THEN
        RAISE EXCEPTION 'authorization relay finish lost its claim' USING ERRCODE='55000';
    END IF;
    DELETE FROM account_approval_authorization.authorization_delivery_pending WHERE event_id=p_event;
END $$;

CREATE FUNCTION account_approval_authorization.authorization_relay_outcome(p_event UUID,p_token UUID,p_epoch BIGINT,p_worker TEXT)
RETURNS TABLE(outcome TEXT,attempts INTEGER) LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
  IF session_user<>'account_approval_relay' THEN RAISE EXCEPTION 'authorization relay identity required' USING ERRCODE='42501'; END IF;
  RETURN QUERY SELECT CASE WHEN o.published_at IS NOT NULL THEN 'PUBLISHED'
                           WHEN o.dead_lettered_at IS NOT NULL THEN 'DEAD_LETTERED'
                           WHEN o.locked_by=p_worker THEN 'CLAIMED' ELSE 'RETRY' END,o.attempts
    FROM public.outbox_events o WHERE o.event_id=p_event AND o.relay_claim_token=p_token AND o.relay_control_epoch=p_epoch AND o.relay_claim_worker=p_worker;
END $$;

CREATE FUNCTION account_approval_authorization.authorization_relay_readiness()
RETURNS TABLE(pending BIGINT,active_claims BIGINT,reclaimable_claims BIGINT,unclassified_dead_rows BIGINT)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE control_row RECORD; invalid_count BIGINT; now_value TIMESTAMPTZ := pg_catalog.clock_timestamp();
BEGIN
  IF session_user<>'account_approval_relay' THEN RAISE EXCEPTION 'authorization relay identity required' USING ERRCODE='42501'; END IF;
	SELECT * INTO STRICT control_row FROM account_approval_control.lock_admin_approval_control_shared();
	SELECT count(*) INTO invalid_count
	FROM public.outbox_events o
	LEFT JOIN account_approval_authorization.approval_authorization_event_bytes s ON s.event_id=o.event_id
	WHERE o.event_type='account.approval-command.authorized.v1' AND (
		o.approval_control_epoch IS NULL OR o.approval_control_epoch<=0
		OR (o.locked_by IS NULL)<>(o.locked_until IS NULL)
		OR (o.relay_claim_token IS NULL)<>(o.relay_control_epoch IS NULL)
		OR (o.relay_claim_token IS NULL)<>(o.relay_claim_worker IS NULL)
		OR (o.published_at IS NOT NULL AND o.dead_lettered_at IS NOT NULL)
		OR ((o.published_at IS NOT NULL OR o.dead_lettered_at IS NOT NULL) AND o.locked_by IS NOT NULL)
		OR (o.published_at IS NULL AND o.dead_lettered_at IS NULL AND (
			s.event_id IS NULL OR s.provenance NOT IN ('FROZEN_AT_APPROVAL','RECONSTRUCTED_FOR_DELIVERY')
			OR s.delivery_bytes IS NULL OR s.event_digest IS NULL
			OR s.event_digest<>pg_catalog.encode(pg_catalog.sha256(s.delivery_bytes),'hex')
			OR NOT account_approval_authorization.authorization_delivery_matches(o.event_id,o.event_type,o.aggregate_id,o.aggregate_version,
				o.payload,o.trace_id,o.occurred_at,o.approval_control_epoch,s.serialization_version,s.delivery_bytes)
			OR (control_row.authorization_relay_state='RUNNING'
				AND NOT account_approval_authorization.authorization_relay_mode_allows(control_row.mode,control_row.authorization_relay_state,
					control_row.approval_epoch,control_row.authorization_relay_epoch,o.approval_control_epoch))
			OR (control_row.authorization_relay_state='FENCED' AND o.locked_by IS NOT NULL)
			OR (o.locked_by IS NOT NULL AND o.relay_claim_token IS NULL AND NOT (
				control_row.authorization_relay_state='RUNNING'
				AND control_row.authorization_relay_epoch=1
				AND o.approval_control_epoch=1
				AND ((control_row.mode='LEGACY' AND control_row.approval_epoch=1) OR control_row.mode='QUIESCING')
			))
			OR (o.locked_by IS NOT NULL AND o.relay_claim_token IS NOT NULL AND (o.relay_claim_worker IS DISTINCT FROM o.locked_by
				OR o.relay_control_epoch IS DISTINCT FROM control_row.authorization_relay_epoch))
		))
	);
	IF invalid_count<>0 THEN RAISE EXCEPTION 'authorization relay persisted facts are invalid' USING ERRCODE='55000'; END IF;
  RETURN QUERY SELECT
    count(*) FILTER(WHERE published_at IS NULL AND dead_lettered_at IS NULL),
    count(*) FILTER(WHERE published_at IS NULL AND dead_lettered_at IS NULL AND locked_by IS NOT NULL AND locked_until>=now_value),
    count(*) FILTER(WHERE published_at IS NULL AND dead_lettered_at IS NULL AND locked_by IS NOT NULL AND locked_until<now_value),
    count(*) FILTER(WHERE dead_lettered_at IS NOT NULL)
  FROM public.outbox_events WHERE event_type='account.approval-command.authorized.v1';
END $$;

CREATE FUNCTION account_approval_authorization.guard_authorization_replay_insert() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
  IF EXISTS(SELECT 1 FROM public.outbox_events WHERE event_id=NEW.event_id AND event_type='account.approval-command.authorized.v1') THEN
    RAISE EXCEPTION 'AUTHORIZATION_EVENT_REPLAY_FORBIDDEN' USING ERRCODE='55000';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER event_replay_authorization_guard BEFORE INSERT ON public.event_replay_requests
FOR EACH ROW EXECUTE FUNCTION account_approval_authorization.guard_authorization_replay_insert();

CREATE FUNCTION public.inspect_authorization_dead_letter(p_event UUID)
RETURNS TABLE(event_digest TEXT) LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
	IF session_user<>'account_service' THEN RAISE EXCEPTION 'account service identity required' USING ERRCODE='42501'; END IF;
	RETURN QUERY SELECT s.event_digest FROM public.outbox_events o
	JOIN account_approval_authorization.approval_authorization_event_bytes s ON s.event_id=o.event_id
	WHERE o.event_id=p_event AND o.event_type='account.approval-command.authorized.v1'
	  AND o.published_at IS NULL AND o.dead_lettered_at IS NOT NULL;
END $$;
REVOKE ALL ON FUNCTION public.inspect_authorization_dead_letter(UUID) FROM PUBLIC,account_approval_relay,account_approval_writer,account_approval_backfill;
GRANT EXECUTE ON FUNCTION public.inspect_authorization_dead_letter(UUID) TO account_service;
REVOKE CREATE ON SCHEMA public FROM account_approval_authorization_owner;

REVOKE ALL ON account_approval_authorization.authorization_delivery_pending FROM PUBLIC,account_service,account_approval_writer,account_approval_backfill,account_approval_relay;
REVOKE ALL ON FUNCTION
 account_approval_authorization.reject_unfinished_delivery(),
 account_approval_authorization.authorization_relay_mode_allows(TEXT,TEXT,BIGINT,BIGINT,BIGINT),
 account_approval_authorization.claim_authorization_events(TEXT,INTEGER,INTEGER),
 account_approval_authorization.open_authorization_delivery(UUID,UUID,BIGINT,TEXT),
 account_approval_authorization.finish_authorization_delivery(UUID,UUID,BIGINT,TEXT,TEXT,TIMESTAMPTZ,TEXT),
 account_approval_authorization.authorization_relay_outcome(UUID,UUID,BIGINT,TEXT),
 account_approval_authorization.authorization_relay_readiness(),
 account_approval_authorization.guard_authorization_replay_insert()
 FROM PUBLIC,account_service,account_approval_writer,account_approval_backfill,account_approval_relay;
GRANT USAGE ON SCHEMA account_approval_authorization TO account_approval_relay;
GRANT EXECUTE ON FUNCTION account_approval_authorization.claim_authorization_events(TEXT,INTEGER,INTEGER),
 account_approval_authorization.open_authorization_delivery(UUID,UUID,BIGINT,TEXT),
 account_approval_authorization.finish_authorization_delivery(UUID,UUID,BIGINT,TEXT,TEXT,TIMESTAMPTZ,TEXT),
 account_approval_authorization.authorization_relay_outcome(UUID,UUID,BIGINT,TEXT),
 account_approval_authorization.authorization_relay_readiness() TO account_approval_relay;

REVOKE ALL ON public.event_replay_requests FROM PUBLIC,account_service,account_approval_relay,account_approval_writer,account_approval_backfill;
GRANT SELECT ON public.event_replay_requests TO account_service;
GRANT INSERT(replay_id,event_id,event_digest,idempotency_key,request_hash,actor_type,service_name,job_type,job_run_id,reason,replayed_at)
 ON public.event_replay_requests TO account_service;

RESET ROLE;
SET ROLE account_approval_authorization_owner;
DO $$
DECLARE relay_oid OID := 'account_approval_relay'::regrole;
BEGIN
  IF pg_catalog.pg_has_role('account_service','account_approval_authorization_owner','MEMBER')
     OR pg_catalog.pg_has_role('account_service','account_approval_relay','MEMBER')
	 OR pg_catalog.pg_get_userbyid((SELECT relowner FROM pg_catalog.pg_class WHERE oid='public.event_replay_requests'::regclass))<>'account_approval_authorization_owner'
	 OR pg_catalog.pg_get_userbyid((SELECT proowner FROM pg_catalog.pg_proc WHERE oid='public.reject_event_replay_audit_mutation()'::regprocedure))<>'account_approval_authorization_owner'
	 OR pg_catalog.pg_get_userbyid((SELECT proowner FROM pg_catalog.pg_proc WHERE oid='public.inspect_authorization_dead_letter(uuid)'::regprocedure))<>'account_approval_authorization_owner'
	 OR pg_catalog.to_regclass('public.outbox_events_authorization_relay_idx') IS NULL
	 OR pg_catalog.to_regclass('public.outbox_events_authorization_relay_claim_token_uidx') IS NULL
	 OR NOT pg_catalog.has_function_privilege('account_service','public.inspect_authorization_dead_letter(uuid)','EXECUTE')
     OR pg_catalog.has_table_privilege('account_approval_relay','public.outbox_events','SELECT,INSERT,UPDATE,DELETE,TRUNCATE,TRIGGER')
	 OR pg_catalog.has_schema_privilege('account_approval_relay','account_approval_authorization','CREATE')
	 OR EXISTS(SELECT 1 FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
		JOIN pg_catalog.pg_roles owner_role ON owner_role.oid=p.proowner
		WHERE p.oid IN (
		 'account_approval_authorization.claim_authorization_events(text,integer,integer)'::regprocedure,
		 'account_approval_authorization.open_authorization_delivery(uuid,uuid,bigint,text)'::regprocedure,
		 'account_approval_authorization.finish_authorization_delivery(uuid,uuid,bigint,text,text,timestamp with time zone,text)'::regprocedure,
		 'account_approval_authorization.authorization_relay_outcome(uuid,uuid,bigint,text)'::regprocedure,
		 'account_approval_authorization.authorization_relay_readiness()'::regprocedure)
		AND (owner_role.rolname<>'account_approval_authorization_owner' OR NOT p.prosecdef
		 OR NOT COALESCE(p.proconfig @> ARRAY['search_path=pg_catalog']::text[],false)))
	 OR EXISTS(SELECT 1 FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
		WHERE n.nspname='account_approval_authorization'
		 AND pg_catalog.has_function_privilege('account_approval_relay',p.oid,'EXECUTE')
		 AND p.oid NOT IN (
		 'account_approval_authorization.claim_authorization_events(text,integer,integer)'::regprocedure,
		 'account_approval_authorization.open_authorization_delivery(uuid,uuid,bigint,text)'::regprocedure,
		 'account_approval_authorization.finish_authorization_delivery(uuid,uuid,bigint,text,text,timestamp with time zone,text)'::regprocedure,
		 'account_approval_authorization.authorization_relay_outcome(uuid,uuid,bigint,text)'::regprocedure,
		 'account_approval_authorization.authorization_relay_readiness()'::regprocedure))
	 OR EXISTS(SELECT 1 FROM pg_catalog.pg_auth_members WHERE roleid=relay_oid OR member=relay_oid) THEN
    RAISE EXCEPTION '000052 final role or ACL verification failed';
  END IF;
END $$;
RESET ROLE;
SET ROLE account_service;
