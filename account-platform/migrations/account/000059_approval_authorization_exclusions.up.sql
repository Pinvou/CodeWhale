-- Classify an authorization dead letter only after the request is terminal and
-- the application has persisted an immutable, effect-absent audit fact.  The
-- original Outbox row and its dead-letter state remain unchanged forever.
DO $$
BEGIN
  IF session_user<>'account_migrator' OR current_user<>'account_service' THEN
    RAISE EXCEPTION '000059 requires the isolated account migration identities' USING ERRCODE='42501';
  END IF;
  IF NOT EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=58) THEN
    RAISE EXCEPTION '000059 requires the complete 000058 approval consumer schema' USING ERRCODE='55000';
  END IF;
END $$;

GRANT CREATE ON SCHEMA public TO account_approval_authorization_owner;
GRANT SELECT(id,action,resource_type,resource_id,result,change_summary,occurred_at)
  ON public.audit_logs TO account_approval_authorization_owner;

RESET ROLE;
SET ROLE account_approval_authorization_owner;

CREATE TABLE account_approval_authorization.approval_authorization_delivery_exclusions (
  schema_version SMALLINT NOT NULL CHECK(schema_version=1),
  event_id UUID PRIMARY KEY,
  event_digest TEXT NOT NULL CHECK(event_digest~'^[0-9a-f]{64}$'),
  event_approval_epoch BIGINT NOT NULL CHECK(event_approval_epoch>0),
  request_id UUID NOT NULL,
  request_version BIGINT NOT NULL CHECK(request_version>0),
  request_status TEXT NOT NULL CHECK(request_status='INVALIDATED'),
  sealed_at_control_approval_epoch BIGINT NOT NULL CHECK(sealed_at_control_approval_epoch>0),
  reason_code TEXT NOT NULL CHECK(reason_code IN ('CUTOVER_INVALIDATED_NO_EXECUTION','SAFE_ROLLBACK_INVALIDATED_NO_EXECUTION')),
  verified_at TIMESTAMPTZ NOT NULL,
  audit_ref UUID NOT NULL UNIQUE
);

CREATE FUNCTION account_approval_authorization.reject_authorization_exclusion_mutation() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
  RAISE EXCEPTION 'approval authorization exclusions are immutable' USING ERRCODE='55000';
END $$;
CREATE TRIGGER approval_authorization_exclusions_immutable
BEFORE UPDATE OR DELETE ON account_approval_authorization.approval_authorization_delivery_exclusions
FOR EACH ROW EXECUTE FUNCTION account_approval_authorization.reject_authorization_exclusion_mutation();
CREATE TRIGGER approval_authorization_exclusions_no_truncate
BEFORE TRUNCATE ON account_approval_authorization.approval_authorization_delivery_exclusions
FOR EACH STATEMENT EXECUTE FUNCTION account_approval_authorization.reject_authorization_exclusion_mutation();

CREATE FUNCTION account_approval_authorization.authorization_exclusion_matches(p_event UUID)
RETURNS BOOLEAN LANGUAGE sql STABLE SET search_path=pg_catalog AS $$
  SELECT EXISTS(
    SELECT 1
      FROM account_approval_authorization.approval_authorization_delivery_exclusions x
      JOIN public.outbox_events o ON o.event_id=x.event_id
      JOIN account_approval_authorization.approval_authorization_event_bytes s ON s.event_id=o.event_id
      JOIN public.approval_requests r ON r.id=x.request_id
      JOIN public.audit_logs a ON a.id=x.audit_ref
     WHERE x.event_id=p_event
       AND o.event_type='account.approval-command.authorized.v1'
       AND o.aggregate_id=x.request_id
       AND o.approval_control_epoch=x.event_approval_epoch
       AND o.published_at IS NULL AND o.dead_lettered_at IS NOT NULL
       AND o.locked_by IS NULL AND o.locked_until IS NULL
       AND s.event_digest=x.event_digest
       AND s.provenance IN ('FROZEN_AT_APPROVAL','RECONSTRUCTED_FOR_DELIVERY')
       AND s.delivery_bytes IS NOT NULL
       AND s.event_digest=pg_catalog.encode(pg_catalog.sha256(s.delivery_bytes),'hex')
       AND r.id=o.aggregate_id AND r.status=x.request_status AND r.version=x.request_version
       AND NOT EXISTS(SELECT 1 FROM public.approval_executions e WHERE e.approval_request_id=r.id)
       AND a.action='approval.authorization-exclusion.effect-verified'
       AND a.resource_type='approval_authorization_event'
       AND a.resource_id=x.event_id::TEXT AND a.result='SUCCEEDED'
       AND a.occurred_at=x.verified_at
       AND a.change_summary->>'request_id'=x.request_id::TEXT
       AND a.change_summary->>'event_digest'=x.event_digest
       AND a.change_summary->>'effect_absent'='true'
  )
$$;
REVOKE ALL ON FUNCTION account_approval_authorization.authorization_exclusion_matches(UUID)
  FROM PUBLIC,account_service,account_approval_writer,account_approval_backfill,account_approval_relay;

CREATE FUNCTION account_approval_authorization.seal_authorization_delivery_exclusion(
  p_event UUID,p_expected_request_version BIGINT,p_reason TEXT,p_audit_ref UUID
) RETURNS VOID LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE control_row RECORD; outbox_row RECORD; request_row RECORD; digest_value TEXT; audit_row RECORD; existing_row RECORD;
BEGIN
  IF session_user<>'account_service' OR p_event IS NULL OR p_expected_request_version IS NULL OR p_expected_request_version<1
     OR p_audit_ref IS NULL OR p_reason NOT IN ('CUTOVER_INVALIDATED_NO_EXECUTION','SAFE_ROLLBACK_INVALIDATED_NO_EXECUTION') THEN
    RAISE EXCEPTION 'authorization exclusion request is invalid' USING ERRCODE='22023';
  END IF;
  SELECT * INTO STRICT control_row FROM account_approval_control.lock_admin_approval_control_shared();
  IF (control_row.mode='QUIESCING' AND p_reason<>'CUTOVER_INVALIDATED_NO_EXECUTION')
     OR (control_row.mode='SAFE_ROLLBACK' AND p_reason<>'SAFE_ROLLBACK_INVALIDATED_NO_EXECUTION')
     OR control_row.mode NOT IN ('QUIESCING','SAFE_ROLLBACK') THEN
    RAISE EXCEPTION 'authorization exclusion control does not allow sealing' USING ERRCODE='55000';
  END IF;
  SELECT o.aggregate_id,o.approval_control_epoch,o.published_at,o.dead_lettered_at,o.locked_by,o.locked_until
    INTO STRICT outbox_row FROM public.outbox_events o WHERE o.event_id=p_event
      AND o.event_type='account.approval-command.authorized.v1' FOR UPDATE;
  SELECT r.status,r.version INTO STRICT request_row FROM public.approval_requests r
    WHERE r.id=outbox_row.aggregate_id FOR UPDATE;
  SELECT s.event_digest INTO STRICT digest_value
    FROM account_approval_authorization.approval_authorization_event_bytes s
   WHERE s.event_id=p_event AND s.provenance IN ('FROZEN_AT_APPROVAL','RECONSTRUCTED_FOR_DELIVERY')
     AND s.delivery_bytes IS NOT NULL AND s.event_digest=pg_catalog.encode(pg_catalog.sha256(s.delivery_bytes),'hex');
  SELECT a.action,a.resource_type,a.resource_id,a.result,a.change_summary,a.occurred_at INTO STRICT audit_row
    FROM public.audit_logs a WHERE a.id=p_audit_ref;
  IF outbox_row.approval_control_epoch IS NULL OR outbox_row.approval_control_epoch<1
     OR outbox_row.published_at IS NOT NULL OR outbox_row.dead_lettered_at IS NULL
     OR outbox_row.locked_by IS NOT NULL OR outbox_row.locked_until IS NOT NULL
     OR request_row.status<>'INVALIDATED' OR request_row.version<>p_expected_request_version
     OR EXISTS(SELECT 1 FROM public.approval_executions e WHERE e.approval_request_id=outbox_row.aggregate_id)
     OR audit_row.action<>'approval.authorization-exclusion.effect-verified'
     OR audit_row.resource_type<>'approval_authorization_event' OR audit_row.resource_id<>p_event::TEXT
     OR audit_row.result<>'SUCCEEDED' OR audit_row.occurred_at>pg_catalog.clock_timestamp()
     OR audit_row.change_summary->>'request_id'<>outbox_row.aggregate_id::TEXT
     OR audit_row.change_summary->>'event_digest'<>digest_value
     OR audit_row.change_summary->>'effect_absent'<>'true' THEN
    RAISE EXCEPTION 'authorization exclusion facts are not terminal and effect-free' USING ERRCODE='55000';
  END IF;
  SELECT * INTO existing_row FROM account_approval_authorization.approval_authorization_delivery_exclusions WHERE event_id=p_event;
  IF FOUND THEN
    IF existing_row.schema_version<>1 OR existing_row.event_digest<>digest_value
       OR existing_row.event_approval_epoch<>outbox_row.approval_control_epoch
       OR existing_row.request_id<>outbox_row.aggregate_id OR existing_row.request_version<>request_row.version
       OR existing_row.request_status<>'INVALIDATED' OR existing_row.reason_code<>p_reason
       OR existing_row.audit_ref<>p_audit_ref OR existing_row.verified_at<>audit_row.occurred_at THEN
      RAISE EXCEPTION 'authorization exclusion conflicts with immutable history' USING ERRCODE='55000';
    END IF;
    RETURN;
  END IF;
  INSERT INTO account_approval_authorization.approval_authorization_delivery_exclusions(
    schema_version,event_id,event_digest,event_approval_epoch,request_id,request_version,request_status,
    sealed_at_control_approval_epoch,reason_code,verified_at,audit_ref)
  VALUES(1,p_event,digest_value,outbox_row.approval_control_epoch,outbox_row.aggregate_id,request_row.version,'INVALIDATED',
    control_row.approval_epoch,p_reason,audit_row.occurred_at,p_audit_ref);
END $$;
REVOKE ALL ON FUNCTION account_approval_authorization.seal_authorization_delivery_exclusion(UUID,BIGINT,TEXT,UUID)
  FROM PUBLIC,account_service,account_approval_writer,account_approval_backfill,account_approval_relay;

CREATE OR REPLACE FUNCTION account_approval_authorization.authorization_relay_readiness()
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
        control_row.authorization_relay_state='RUNNING' AND control_row.authorization_relay_epoch=1
        AND o.approval_control_epoch=1 AND ((control_row.mode='LEGACY' AND control_row.approval_epoch=1) OR control_row.mode='QUIESCING')))
      OR (o.locked_by IS NOT NULL AND o.relay_claim_token IS NOT NULL AND (o.relay_claim_worker IS DISTINCT FROM o.locked_by
        OR o.relay_control_epoch IS DISTINCT FROM control_row.authorization_relay_epoch))
    ))
  );
  IF invalid_count<>0 THEN RAISE EXCEPTION 'authorization relay persisted facts are invalid' USING ERRCODE='55000'; END IF;
  RETURN QUERY SELECT
    count(*) FILTER(WHERE o.published_at IS NULL AND o.dead_lettered_at IS NULL),
    count(*) FILTER(WHERE o.published_at IS NULL AND o.dead_lettered_at IS NULL AND o.locked_by IS NOT NULL AND o.locked_until>=now_value),
    count(*) FILTER(WHERE o.published_at IS NULL AND o.dead_lettered_at IS NULL AND o.locked_by IS NOT NULL AND o.locked_until<now_value),
    count(*) FILTER(WHERE o.dead_lettered_at IS NOT NULL
      AND NOT account_approval_authorization.authorization_exclusion_matches(o.event_id))
  FROM public.outbox_events o WHERE o.event_type='account.approval-command.authorized.v1';
END $$;

CREATE FUNCTION public.seal_approval_authorization_exclusion(p_event UUID,p_expected_request_version BIGINT,p_reason TEXT,p_audit_ref UUID)
RETURNS VOID LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog AS $$
  SELECT account_approval_authorization.seal_authorization_delivery_exclusion(p_event,p_expected_request_version,p_reason,p_audit_ref)
$$;
REVOKE ALL ON FUNCTION public.seal_approval_authorization_exclusion(UUID,BIGINT,TEXT,UUID) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.seal_approval_authorization_exclusion(UUID,BIGINT,TEXT,UUID) TO account_service;

REVOKE ALL ON account_approval_authorization.approval_authorization_delivery_exclusions
  FROM PUBLIC,account_service,account_approval_writer,account_approval_backfill,account_approval_relay;
REVOKE ALL ON FUNCTION account_approval_authorization.reject_authorization_exclusion_mutation(),
  account_approval_authorization.authorization_exclusion_matches(UUID)
  FROM PUBLIC,account_service,account_approval_writer,account_approval_backfill,account_approval_relay;
REVOKE ALL ON FUNCTION account_approval_authorization.authorization_relay_readiness()
  FROM PUBLIC,account_service,account_approval_writer,account_approval_backfill,account_approval_relay;
GRANT EXECUTE ON FUNCTION account_approval_authorization.authorization_relay_readiness() TO account_approval_relay;

RESET ROLE;
SET ROLE account_service;
REVOKE CREATE ON SCHEMA public FROM account_approval_authorization_owner;

RESET ROLE;
SET ROLE account_approval_authorization_owner;
DO $$
BEGIN
  IF pg_catalog.has_schema_privilege('account_approval_authorization_owner','public','CREATE')
     OR pg_catalog.has_table_privilege('account_service','account_approval_authorization.approval_authorization_delivery_exclusions','SELECT')
     OR pg_catalog.has_function_privilege('public','public.seal_approval_authorization_exclusion(uuid,bigint,text,uuid)','EXECUTE')
     OR NOT pg_catalog.has_function_privilege('account_service','public.seal_approval_authorization_exclusion(uuid,bigint,text,uuid)','EXECUTE')
     OR pg_catalog.has_function_privilege('account_service','account_approval_authorization.seal_authorization_delivery_exclusion(uuid,bigint,text,uuid)','EXECUTE')
     OR NOT pg_catalog.has_function_privilege('account_approval_relay','account_approval_authorization.authorization_relay_readiness()','EXECUTE') THEN
    RAISE EXCEPTION '000059 approval exclusion privilege inventory is invalid' USING ERRCODE='55000';
  END IF;
END $$;

RESET ROLE;
SET ROLE account_service;
