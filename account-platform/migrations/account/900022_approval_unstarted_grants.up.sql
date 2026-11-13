-- Read-only investigation of a granted attempt with no Account execution.
-- Absence in this snapshot is never a proof of zero domain/financial effect.
DO $$ BEGIN
  IF session_user<>'account_migrator' OR current_user<>'account_service' THEN
    RAISE EXCEPTION '900022 requires isolated migration identities' USING ERRCODE='42501';
  END IF;
END $$;
GRANT SELECT(command_fingerprint,execution_deadline) ON public.approval_requests TO account_approval_authorization_owner;
GRANT CREATE ON SCHEMA public TO account_approval_authorization_owner;
RESET ROLE;
SET ROLE account_approval_authorization_owner;
CREATE INDEX approval_unstarted_grants_inventory_idx
  ON account_approval_authorization.approval_delivery_states(original_event_id,durable)
  WHERE state='DOMAIN_INFLIGHT' AND serialization_version='APPROVAL_RFC8785_DECIMAL_V1'
    AND executor_identity='billing-command-executor';

CREATE FUNCTION public.list_approval_unstarted_grants(p_limit INTEGER,p_after_event UUID,p_after_durable TEXT)
RETURNS TABLE(original_event_id UUID,durable TEXT,approval_request_id UUID,command_type TEXT,
  policy_version BIGINT,request_status TEXT,request_version BIGINT,command_fingerprint TEXT,
  execution_deadline TIMESTAMPTZ,delivery_version BIGINT,domain_attempt_count SMALLINT)
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
  IF session_user<>'account_service' THEN
    RAISE EXCEPTION 'unstarted grant inventory forbidden' USING ERRCODE='42501';
  END IF;
  IF p_limit IS NULL OR p_limit<1 OR p_limit>201
    OR (p_after_event IS NULL)<>(p_after_durable IS NULL)
    OR (p_after_durable IS NOT NULL AND (char_length(p_after_durable) NOT BETWEEN 1 AND 120
      OR p_after_durable<>btrim(p_after_durable) OR p_after_durable~'[[:cntrl:]]')) THEN
    RAISE EXCEPTION 'invalid unstarted grant inventory arguments' USING ERRCODE='22023';
  END IF;
  RETURN QUERY SELECT d.original_event_id,d.durable,d.approval_request_id,d.command_type,
    d.policy_version,r.status,r.version,pg_catalog.encode(r.command_fingerprint,'hex'),r.execution_deadline,d.version,d.domain_attempt_count
  FROM account_approval_authorization.approval_delivery_states d
  JOIN public.approval_requests r ON r.id=d.approval_request_id
  WHERE d.state='DOMAIN_INFLIGHT' AND d.serialization_version='APPROVAL_RFC8785_DECIMAL_V1'
    AND d.executor_identity='billing-command-executor'
    AND (p_after_event IS NULL OR (d.original_event_id,d.durable)>(p_after_event,p_after_durable))
    AND NOT EXISTS(SELECT 1 FROM public.approval_executions e WHERE e.approval_request_id=d.approval_request_id)
  ORDER BY d.original_event_id,d.durable LIMIT p_limit;
END $$;
REVOKE ALL ON FUNCTION public.list_approval_unstarted_grants(INTEGER,UUID,TEXT) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.list_approval_unstarted_grants(INTEGER,UUID,TEXT) TO account_service;
RESET ROLE;
SET ROLE account_service;
REVOKE CREATE ON SCHEMA public FROM account_approval_authorization_owner;
