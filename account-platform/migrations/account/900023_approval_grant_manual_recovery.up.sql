-- Manual, dual-human recovery records are distinct from executions and ACKs.
DO $$ BEGIN
 IF session_user<>'account_migrator' OR current_user<>'account_service' THEN
  RAISE EXCEPTION '900023 requires isolated migration identities' USING ERRCODE='42501';
 END IF;
END $$;
GRANT CREATE ON SCHEMA public TO account_approval_authorization_owner;
GRANT SELECT(command_fingerprint,execution_deadline,updated_at) ON public.approval_requests TO account_approval_authorization_owner;
GRANT UPDATE(updated_at) ON public.approval_requests TO account_approval_authorization_owner;
GRANT SELECT(requester_permission,approver_permission) ON public.approval_command_policies TO account_approval_authorization_owner;
GRANT SELECT(id,domain,case_type,resource_type,resource_id) ON public.manual_review_cases TO account_approval_authorization_owner;
GRANT REFERENCES(id) ON public.manual_review_cases TO account_approval_authorization_owner;
RESET ROLE;
SET ROLE account_approval_authorization_owner;

CREATE TABLE account_approval_authorization.grant_recovery_cases (
 id UUID PRIMARY KEY REFERENCES public.manual_review_cases(id) ON DELETE RESTRICT,
 original_event_id UUID NOT NULL,
 durable TEXT NOT NULL,
 approval_request_id UUID NOT NULL,
 command_type TEXT NOT NULL,
 policy_version BIGINT NOT NULL,
 approved_request_version BIGINT NOT NULL,
 approval_epoch BIGINT NOT NULL,
 event_digest TEXT NOT NULL,
 command_fingerprint TEXT NOT NULL CHECK(command_fingerprint~'^[0-9a-f]{64}$'),
 status TEXT NOT NULL CHECK(status IN ('OPEN','PROPOSED','COMPLETED')),
 version BIGINT NOT NULL CHECK(version>0),
 proposal_id UUID,
 proposer_id UUID,
 proposer_binding_id UUID,
 proposer_authorization_version BIGINT,
 reviewer_id UUID,
 last_completion_id UUID,
 created_at TIMESTAMPTZ NOT NULL,
 updated_at TIMESTAMPTZ NOT NULL,
 UNIQUE(original_event_id,durable),
 FOREIGN KEY(original_event_id,durable) REFERENCES account_approval_authorization.approval_delivery_states(original_event_id,durable) ON DELETE RESTRICT,
 CHECK((status='PROPOSED')=(proposal_id IS NOT NULL)),
 CHECK(status<>'PROPOSED' OR (proposer_id IS NOT NULL AND proposer_binding_id IS NOT NULL AND proposer_authorization_version>0)),
 CHECK(status<>'COMPLETED' OR (last_completion_id IS NOT NULL AND reviewer_id IS NOT NULL AND reviewer_id<>proposer_id))
);
CREATE INDEX grant_recovery_cases_work_idx ON account_approval_authorization.grant_recovery_cases(status,created_at,id);
CREATE INDEX grant_recovery_cases_request_idx ON account_approval_authorization.grant_recovery_cases(approval_request_id);
CREATE INDEX grant_recovery_admission_idx ON account_approval_authorization.approval_delivery_states(approval_request_id,original_event_id,durable)
 WHERE state='DOMAIN_INFLIGHT' AND serialization_version='APPROVAL_RFC8785_DECIMAL_V1' AND executor_identity='billing-command-executor';
CREATE TABLE account_approval_authorization.grant_recovery_operations (
 case_id UUID NOT NULL REFERENCES account_approval_authorization.grant_recovery_cases(id) ON DELETE RESTRICT,
 operation_id UUID NOT NULL,
 case_version BIGINT NOT NULL,
 action TEXT NOT NULL CHECK(action IN ('CREATED','OBSERVE','PROPOSE','REVIEW')),
 actor_id UUID,
 expected_version BIGINT NOT NULL,
 payload JSONB NOT NULL CHECK(jsonb_typeof(payload)='object' AND octet_length(payload::text)<=4096),
 response JSONB NOT NULL CHECK(jsonb_typeof(response)='object'),
 created_at TIMESTAMPTZ NOT NULL,
 PRIMARY KEY(case_id,operation_id),
 UNIQUE(case_id,case_version)
);
REVOKE ALL ON account_approval_authorization.grant_recovery_cases,account_approval_authorization.grant_recovery_operations FROM PUBLIC,account_service,account_approval_writer,account_approval_backfill,account_approval_relay;

CREATE FUNCTION account_approval_authorization.protect_grant_recovery_case() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF (NEW.id,NEW.original_event_id,NEW.durable,NEW.approval_request_id,NEW.command_type,NEW.policy_version,
     NEW.approved_request_version,NEW.approval_epoch,NEW.event_digest,NEW.command_fingerprint,NEW.created_at)
 IS DISTINCT FROM (OLD.id,OLD.original_event_id,OLD.durable,OLD.approval_request_id,OLD.command_type,OLD.policy_version,
     OLD.approved_request_version,OLD.approval_epoch,OLD.event_digest,OLD.command_fingerprint,OLD.created_at)
 OR NEW.version<>OLD.version+1 THEN
  RAISE EXCEPTION 'manual recovery binding/version is immutable' USING ERRCODE='55000';
 END IF;
 RETURN NEW;
END $$;
REVOKE ALL ON FUNCTION account_approval_authorization.protect_grant_recovery_case() FROM PUBLIC;
CREATE TRIGGER grant_recovery_case_guard BEFORE UPDATE ON account_approval_authorization.grant_recovery_cases
 FOR EACH ROW EXECUTE FUNCTION account_approval_authorization.protect_grant_recovery_case();
CREATE TRIGGER grant_recovery_case_no_remove BEFORE DELETE OR TRUNCATE ON account_approval_authorization.grant_recovery_cases
 FOR EACH STATEMENT EXECUTE FUNCTION account_approval_authorization.reject_approval_delivery_state_removal();
CREATE TRIGGER grant_recovery_operations_immutable BEFORE UPDATE OR DELETE ON account_approval_authorization.grant_recovery_operations
 FOR EACH ROW EXECUTE FUNCTION account_approval_authorization.reject_approval_delivery_state_removal();
CREATE TRIGGER grant_recovery_operations_no_truncate BEFORE TRUNCATE ON account_approval_authorization.grant_recovery_operations
 FOR EACH STATEMENT EXECUTE FUNCTION account_approval_authorization.reject_approval_delivery_state_removal();

CREATE FUNCTION public.pending_approval_grant_recovery(p_limit INTEGER)
RETURNS TABLE(original_event_id UUID,durable TEXT,approval_request_id UUID)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 IF session_user<>'account_service' THEN RAISE EXCEPTION 'grant recovery forbidden' USING ERRCODE='42501'; END IF;
 IF p_limit IS NULL OR p_limit<1 OR p_limit>100 THEN RAISE EXCEPTION 'invalid recovery batch' USING ERRCODE='22023'; END IF;
 RETURN QUERY SELECT d.original_event_id,d.durable,d.approval_request_id
 FROM account_approval_authorization.approval_delivery_states d
 JOIN public.approval_requests r ON r.id=d.approval_request_id
 WHERE d.state='DOMAIN_INFLIGHT' AND d.serialization_version='APPROVAL_RFC8785_DECIMAL_V1'
 AND d.executor_identity='billing-command-executor' AND d.durable<>'billing-command-executor-v1'
 AND r.execution_deadline<=statement_timestamp()
 AND NOT EXISTS(SELECT 1 FROM public.approval_executions e WHERE e.approval_request_id=r.id)
 AND NOT EXISTS(SELECT 1 FROM account_approval_authorization.grant_recovery_cases c WHERE c.original_event_id=d.original_event_id AND c.durable=d.durable)
 ORDER BY d.approval_request_id,d.original_event_id,d.durable LIMIT p_limit FOR UPDATE OF r,d SKIP LOCKED;
END $$;

CREATE FUNCTION public.list_approval_grant_recovery(p_after UUID,p_limit INTEGER)
RETURNS SETOF JSONB LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 IF session_user<>'account_service' THEN RAISE EXCEPTION 'grant recovery forbidden' USING ERRCODE='42501'; END IF;
 IF p_limit IS NULL OR p_limit<1 OR p_limit>101 THEN RAISE EXCEPTION 'invalid recovery page' USING ERRCODE='22023'; END IF;
 RETURN QUERY SELECT to_jsonb(c) FROM account_approval_authorization.grant_recovery_cases c
 WHERE p_after IS NULL OR c.id>p_after ORDER BY c.id LIMIT p_limit;
END $$;

CREATE FUNCTION public.lock_approval_grant_recovery(p_case UUID) RETURNS JSONB
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE c account_approval_authorization.grant_recovery_cases;
BEGIN
 IF session_user<>'account_service' THEN RAISE EXCEPTION 'grant recovery forbidden' USING ERRCODE='42501'; END IF;
 SELECT * INTO STRICT c FROM account_approval_authorization.grant_recovery_cases x WHERE x.id=p_case;
 PERFORM r.id FROM public.approval_requests r WHERE r.id=c.approval_request_id FOR UPDATE;
 PERFORM d.original_event_id FROM account_approval_authorization.approval_delivery_states d WHERE d.original_event_id=c.original_event_id AND d.durable=c.durable FOR UPDATE;
 SELECT * INTO STRICT c FROM account_approval_authorization.grant_recovery_cases x WHERE x.id=p_case FOR UPDATE;
 RETURN to_jsonb(c);
END $$;

CREATE FUNCTION public.read_approval_grant_recovery_operations(p_case UUID,p_after BIGINT,p_limit INTEGER)
RETURNS SETOF JSONB LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 IF session_user<>'account_service' THEN RAISE EXCEPTION 'grant recovery forbidden' USING ERRCODE='42501'; END IF;
 IF p_limit IS NULL OR p_limit<1 OR p_limit>101 OR p_after IS NULL OR p_after<0 THEN RAISE EXCEPTION 'invalid recovery history page' USING ERRCODE='22023'; END IF;
 RETURN QUERY SELECT to_jsonb(o) FROM account_approval_authorization.grant_recovery_operations o
 WHERE o.case_id=p_case AND o.case_version>p_after ORDER BY o.case_version LIMIT p_limit;
END $$;

CREATE FUNCTION public.read_approval_grant_recovery_operation(p_case UUID,p_operation UUID) RETURNS JSONB
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE result JSONB;
BEGIN
 IF session_user<>'account_service' THEN RAISE EXCEPTION 'grant recovery forbidden' USING ERRCODE='42501'; END IF;
 SELECT to_jsonb(o) INTO result FROM account_approval_authorization.grant_recovery_operations o WHERE o.case_id=p_case AND o.operation_id=p_operation;
 RETURN result;
END $$;

CREATE FUNCTION account_approval_authorization.grant_recovery_actor_allows(p_actor UUID,p_binding UUID,p_version BIGINT,p_permission TEXT)
RETURNS BOOLEAN LANGUAGE sql SET search_path=pg_catalog AS $$
 SELECT EXISTS(SELECT 1 FROM public.users u JOIN public.role_assignments ra ON ra.user_id=u.id
 JOIN public.role_permissions rp ON rp.role_code=ra.role_code AND rp.permission_code=p_permission
 WHERE u.id=p_actor AND u.status='ACTIVE' AND ra.id=p_binding AND ra.organization_id IS NULL AND ra.revoked_at IS NULL
 AND ra.starts_at<=clock_timestamp() AND (ra.expires_at IS NULL OR ra.expires_at>clock_timestamp())
 AND p_version=COALESCE((SELECT av.version FROM public.authorization_versions av WHERE av.user_id=p_actor AND av.scope='PLATFORM'
  AND av.scope_id='00000000-0000-0000-0000-000000000000'::UUID),1))
$$;
REVOKE ALL ON FUNCTION account_approval_authorization.grant_recovery_actor_allows(UUID,UUID,BIGINT,TEXT) FROM PUBLIC;

CREATE FUNCTION public.apply_approval_grant_recovery(
 p_case UUID,p_operation UUID,p_action TEXT,p_actor UUID,p_expected BIGINT,p_payload JSONB,p_binding UUID,p_auth_version BIGINT
) RETURNS JSONB LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE c account_approval_authorization.grant_recovery_cases; d account_approval_authorization.approval_delivery_states;
 old_operation account_approval_authorization.grant_recovery_operations; result JSONB; proposed JSONB; requester_permission TEXT; approver_permission TEXT;
BEGIN
 IF session_user<>'account_service' THEN RAISE EXCEPTION 'grant recovery forbidden' USING ERRCODE='42501'; END IF;
 IF p_operation IS NULL OR p_actor IS NULL OR p_action IS NULL OR p_action NOT IN ('OBSERVE','PROPOSE','REVIEW')
 OR p_expected IS NULL OR p_expected<1 OR p_payload IS NULL OR jsonb_typeof(p_payload)<>'object' OR octet_length(p_payload::text)>4096
 OR p_binding IS NULL OR p_auth_version IS NULL OR p_auth_version<1 THEN
  RAISE EXCEPTION 'invalid recovery action' USING ERRCODE='22023';
 END IF;
 IF jsonb_typeof(p_payload->'sha256') IS DISTINCT FROM 'string'
 OR (p_action IN ('OBSERVE','PROPOSE') AND jsonb_typeof(p_payload->'reference') IS DISTINCT FROM 'string')
 OR (p_action='PROPOSE' AND (jsonb_typeof(p_payload->'completion_fence_reference') IS DISTINCT FROM 'string'
   OR jsonb_typeof(p_payload->'kind') IS DISTINCT FROM 'string' OR jsonb_typeof(p_payload->'outcome') IS DISTINCT FROM 'string')) THEN
  RAISE EXCEPTION 'typed recovery evidence required' USING ERRCODE='22023';
 END IF;
 SELECT * INTO STRICT c FROM account_approval_authorization.grant_recovery_cases x WHERE x.id=p_case;
 PERFORM r.id FROM public.approval_requests r WHERE r.id=c.approval_request_id FOR UPDATE;
 SELECT * INTO STRICT d FROM account_approval_authorization.approval_delivery_states s WHERE s.original_event_id=c.original_event_id AND s.durable=c.durable FOR UPDATE;
 SELECT * INTO STRICT c FROM account_approval_authorization.grant_recovery_cases x WHERE x.id=p_case FOR UPDATE;
 SELECT p.requester_permission,p.approver_permission INTO STRICT requester_permission,approver_permission FROM public.approval_command_policies p WHERE p.command_type=c.command_type AND p.policy_version=c.policy_version;
 IF NOT ((p_action IN ('OBSERVE','PROPOSE') AND account_approval_authorization.grant_recovery_actor_allows(p_actor,p_binding,p_auth_version,requester_permission))
  OR (p_action IN ('OBSERVE','REVIEW') AND account_approval_authorization.grant_recovery_actor_allows(p_actor,p_binding,p_auth_version,approver_permission))) THEN
  RAISE EXCEPTION 'current recovery authorization required' USING ERRCODE='42501';
 END IF;
 SELECT * INTO old_operation FROM account_approval_authorization.grant_recovery_operations o WHERE o.case_id=p_case AND o.operation_id=p_operation;
 IF FOUND THEN
  IF old_operation.action<>p_action OR old_operation.actor_id IS DISTINCT FROM p_actor OR old_operation.expected_version<>p_expected OR old_operation.payload<>p_payload THEN
   RAISE EXCEPTION 'recovery operation key conflict' USING ERRCODE='55000';
  END IF;
  RETURN old_operation.response;
 END IF;
 IF c.version<>p_expected OR EXISTS(SELECT 1 FROM public.approval_executions e WHERE e.approval_request_id=c.approval_request_id)
 OR (d.original_event_id,d.durable,d.approval_request_id,d.command_type,d.policy_version,d.approved_request_version,d.approval_epoch,d.event_digest)
 IS DISTINCT FROM (c.original_event_id,c.durable,c.approval_request_id,c.command_type,c.policy_version,c.approved_request_version,c.approval_epoch,c.event_digest) THEN
  RAISE EXCEPTION 'recovery facts changed' USING ERRCODE='55000';
 END IF;
 IF p_action='OBSERVE' THEN
  IF p_payload-ARRAY['state','reference','sha256']<>'{}'::JSONB
   OR COALESCE(p_payload->>'state','') NOT IN ('ABSENT','OBSERVED','UNAVAILABLE','EVIDENCE_CONFLICT') THEN
   RAISE EXCEPTION 'invalid recovery observation' USING ERRCODE='22023';
  END IF;
  UPDATE account_approval_authorization.grant_recovery_cases SET status='OPEN',proposal_id=NULL,
   version=version+1,updated_at=clock_timestamp() WHERE id=p_case;
 ELSIF p_action='PROPOSE' THEN
  IF p_payload-ARRAY['authority','original_event_id','approval_request_id','command_fingerprint','approved_request_version','approval_epoch','kind','outcome','reference','sha256','completion_fence_reference','all_observations_resolved']<>'{}'::JSONB
   OR (p_payload->>'authority') IS DISTINCT FROM 'BILLING'
   OR (p_payload->>'original_event_id') IS DISTINCT FROM c.original_event_id::TEXT
   OR (p_payload->>'approval_request_id') IS DISTINCT FROM c.approval_request_id::TEXT
   OR (p_payload->>'command_fingerprint') IS DISTINCT FROM c.command_fingerprint
   OR (p_payload->'approved_request_version') IS DISTINCT FROM to_jsonb(c.approved_request_version)
   OR (p_payload->'approval_epoch') IS DISTINCT FROM to_jsonb(c.approval_epoch)
   OR (p_payload->'all_observations_resolved') IS DISTINCT FROM 'true'::JSONB
   OR NOT COALESCE(((p_payload->>'kind'='AUTHORITATIVE_FINAL_RESULT' AND p_payload->>'outcome' IN ('SUCCEEDED','FAILED'))
     OR (p_payload->>'kind'='ACTION_ENDED_NO_EFFECT' AND p_payload->>'outcome'='NO_EFFECT')),false)
   OR COALESCE(p_payload->>'completion_fence_reference','')!~'^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$' THEN
   RAISE EXCEPTION 'completed authoritative evidence is required' USING ERRCODE='22023';
  END IF;
  UPDATE account_approval_authorization.grant_recovery_cases SET status='PROPOSED',proposal_id=p_operation,proposer_id=p_actor,
   proposer_binding_id=p_binding,proposer_authorization_version=p_auth_version,reviewer_id=NULL,
   version=version+1,updated_at=clock_timestamp() WHERE id=p_case;
 ELSE
  IF p_payload-ARRAY['proposal_id','sha256','independently_verified']<>'{}'::JSONB
   OR p_payload->'independently_verified' IS DISTINCT FROM 'true'::JSONB
   OR c.status<>'PROPOSED' OR c.proposer_id=p_actor OR (p_payload->>'proposal_id') IS DISTINCT FROM c.proposal_id::TEXT
   OR NOT account_approval_authorization.grant_recovery_actor_allows(c.proposer_id,c.proposer_binding_id,c.proposer_authorization_version,requester_permission) THEN
   RAISE EXCEPTION 'independent review of the current proposal is required' USING ERRCODE='55000';
  END IF;
  SELECT o.payload INTO STRICT proposed FROM account_approval_authorization.grant_recovery_operations o WHERE o.case_id=p_case AND o.operation_id=c.proposal_id AND o.action='PROPOSE';
  IF (p_payload->>'sha256') IS DISTINCT FROM (proposed->>'sha256') THEN RAISE EXCEPTION 'review evidence conflict' USING ERRCODE='55000'; END IF;
  IF d.state='DOMAIN_INFLIGHT' THEN
   UPDATE account_approval_authorization.approval_delivery_states SET state='RESULT_CONFIRMED',last_error_code=NULL,version=version+1,updated_at=clock_timestamp()
    WHERE original_event_id=c.original_event_id AND durable=c.durable;
  ELSIF d.state NOT IN ('RESULT_CONFIRMED','TERMINAL_CONFIRMED') OR c.last_completion_id IS NULL THEN
   RAISE EXCEPTION 'delivery cannot accept a manual result' USING ERRCODE='55000';
  END IF;
  UPDATE account_approval_authorization.grant_recovery_cases SET status='COMPLETED',proposal_id=NULL,reviewer_id=p_actor,last_completion_id=p_operation,
   version=version+1,updated_at=clock_timestamp() WHERE id=p_case;
 END IF;
 IF p_action IN ('OBSERVE','PROPOSE') AND (COALESCE(p_payload->>'reference','')!~'^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'
 OR COALESCE(p_payload->>'sha256','')!~'^[0-9a-f]{64}$') THEN RAISE EXCEPTION 'bounded evidence reference and digest required' USING ERRCODE='22023'; END IF;
 SELECT to_jsonb(x) INTO STRICT result FROM account_approval_authorization.grant_recovery_cases x WHERE x.id=p_case;
 INSERT INTO account_approval_authorization.grant_recovery_operations(case_id,operation_id,case_version,action,actor_id,expected_version,payload,response,created_at)
 VALUES(p_case,p_operation,(result->>'version')::BIGINT,p_action,p_actor,p_expected,p_payload,result,clock_timestamp());
 RETURN result;
END $$;

CREATE FUNCTION public.read_approval_grant_recovery(p_case UUID) RETURNS JSONB
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE result JSONB;
BEGIN
 IF session_user<>'account_service' THEN RAISE EXCEPTION 'grant recovery forbidden' USING ERRCODE='42501'; END IF;
 SELECT to_jsonb(c) INTO result FROM account_approval_authorization.grant_recovery_cases c WHERE c.id=p_case;
 IF result IS NULL THEN RAISE EXCEPTION 'grant recovery not found' USING ERRCODE='P0002'; END IF;
 RETURN result;
END $$;

CREATE FUNCTION public.open_approval_grant_recovery(p_case UUID,p_event UUID,p_durable TEXT) RETURNS JSONB
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE d account_approval_authorization.approval_delivery_states; fingerprint TEXT; deadline TIMESTAMPTZ; result JSONB; admitted INTEGER;
BEGIN
 IF session_user<>'account_service' THEN RAISE EXCEPTION 'grant recovery forbidden' USING ERRCODE='42501'; END IF;
 SELECT * INTO STRICT d FROM account_approval_authorization.approval_delivery_states s WHERE s.original_event_id=p_event AND s.durable=p_durable;
 -- Touch the locked tuple without changing its logical values. A stale
 -- RepeatableRead/Serializable INSERT must fail rather than miss this case.
 -- Only the unchanged metadata column is writable by this NOLOGIN owner.
 UPDATE public.approval_requests r SET updated_at=r.updated_at WHERE r.id=d.approval_request_id
 RETURNING encode(r.command_fingerprint,'hex'),r.execution_deadline INTO STRICT fingerprint,deadline;
 SELECT * INTO STRICT d FROM account_approval_authorization.approval_delivery_states s WHERE s.original_event_id=p_event AND s.durable=p_durable FOR UPDATE NOWAIT;
 IF d.state<>'DOMAIN_INFLIGHT' OR d.serialization_version<>'APPROVAL_RFC8785_DECIMAL_V1' OR d.executor_identity<>'billing-command-executor'
 OR d.durable='billing-command-executor-v1' OR d.domain_attempt_count<1 OR deadline IS NULL OR deadline>clock_timestamp()
 OR EXISTS(SELECT 1 FROM public.approval_executions e WHERE e.approval_request_id=d.approval_request_id)
 OR NOT EXISTS(SELECT 1 FROM public.manual_review_cases m WHERE m.id=p_case AND m.domain='ACCOUNT' AND m.case_type='APPROVAL_GRANT_RECOVERY' AND m.resource_type='APPROVAL_AUTHORIZATION' AND m.resource_id=p_event) THEN
  RAISE EXCEPTION 'grant is not eligible for manual recovery' USING ERRCODE='55000';
 END IF;
 INSERT INTO account_approval_authorization.grant_recovery_cases(id,original_event_id,durable,approval_request_id,command_type,policy_version,approved_request_version,approval_epoch,event_digest,command_fingerprint,status,version,created_at,updated_at)
 VALUES(p_case,p_event,p_durable,d.approval_request_id,d.command_type,d.policy_version,d.approved_request_version,d.approval_epoch,d.event_digest,fingerprint,'OPEN',1,clock_timestamp(),clock_timestamp())
 ON CONFLICT(original_event_id,durable) DO NOTHING;
 GET DIAGNOSTICS admitted=ROW_COUNT;
 SELECT to_jsonb(c) INTO STRICT result FROM account_approval_authorization.grant_recovery_cases c WHERE c.original_event_id=p_event AND c.durable=p_durable;
 INSERT INTO account_approval_authorization.grant_recovery_operations(case_id,operation_id,case_version,action,expected_version,payload,response,created_at)
 VALUES((result->>'id')::UUID,p_event,1,'CREATED',0,'{}',result,clock_timestamp()) ON CONFLICT(case_id,operation_id) DO NOTHING;
 RETURN jsonb_build_object('case',result,'created',admitted=1);
END $$;

-- This fence also covers old Account binaries. Lock the request before the
-- case lookup, so concurrent case admission and Begin cannot pass each other.
CREATE FUNCTION public.fence_manual_grant_recovery_execution() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 PERFORM r.id FROM public.approval_requests r WHERE r.id=NEW.approval_request_id FOR UPDATE;
 IF EXISTS(SELECT 1 FROM account_approval_authorization.grant_recovery_cases c WHERE c.approval_request_id=NEW.approval_request_id) THEN
  RAISE EXCEPTION 'manual recovery owns this authorization' USING ERRCODE='55000';
 END IF;
 RETURN NEW;
END $$;
GRANT EXECUTE ON FUNCTION public.fence_manual_grant_recovery_execution() TO account_service;
RESET ROLE;
SET ROLE account_service;
CREATE TRIGGER zz_manual_grant_recovery_execution_fence BEFORE INSERT ON public.approval_executions
 FOR EACH ROW EXECUTE FUNCTION public.fence_manual_grant_recovery_execution();
RESET ROLE;
SET ROLE account_approval_authorization_owner;
REVOKE ALL ON FUNCTION public.fence_manual_grant_recovery_execution() FROM PUBLIC,account_service;
REVOKE ALL ON FUNCTION public.pending_approval_grant_recovery(INTEGER),public.read_approval_grant_recovery(UUID),public.open_approval_grant_recovery(UUID,UUID,TEXT) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.pending_approval_grant_recovery(INTEGER),public.read_approval_grant_recovery(UUID),public.open_approval_grant_recovery(UUID,UUID,TEXT) TO account_service;
REVOKE ALL ON FUNCTION public.list_approval_grant_recovery(UUID,INTEGER),public.read_approval_grant_recovery_operations(UUID,BIGINT,INTEGER),public.apply_approval_grant_recovery(UUID,UUID,TEXT,UUID,BIGINT,JSONB,UUID,BIGINT) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.list_approval_grant_recovery(UUID,INTEGER),public.read_approval_grant_recovery_operations(UUID,BIGINT,INTEGER),public.apply_approval_grant_recovery(UUID,UUID,TEXT,UUID,BIGINT,JSONB,UUID,BIGINT) TO account_service;
REVOKE ALL ON FUNCTION public.read_approval_grant_recovery_operation(UUID,UUID) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.read_approval_grant_recovery_operation(UUID,UUID) TO account_service;
REVOKE ALL ON FUNCTION public.lock_approval_grant_recovery(UUID) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.lock_approval_grant_recovery(UUID) TO account_service;
RESET ROLE;
SET ROLE account_service;
REVOKE CREATE ON SCHEMA public FROM account_approval_authorization_owner;
