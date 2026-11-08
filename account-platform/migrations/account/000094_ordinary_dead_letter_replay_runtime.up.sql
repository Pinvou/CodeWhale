DO $$ BEGIN
    IF session_user <> 'account_migrator' OR current_user <> 'account_service' THEN
        RAISE EXCEPTION 'ordinary replay migration requires the dedicated Account migrator' USING ERRCODE='42501';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname='account_approval_authorization_owner'
        AND NOT rolcanlogin AND NOT rolinherit AND NOT rolsuper AND NOT rolcreatedb
        AND NOT rolcreaterole AND NOT rolreplication AND NOT rolbypassrls)
       OR pg_catalog.pg_get_userbyid((SELECT relowner FROM pg_catalog.pg_class WHERE oid='public.outbox_events'::regclass)) <> 'account_approval_authorization_owner'
       OR pg_catalog.pg_get_userbyid((SELECT relowner FROM pg_catalog.pg_class WHERE oid='public.event_replay_requests'::regclass)) <> 'account_approval_authorization_owner'
       OR pg_catalog.has_schema_privilege('account_approval_authorization_owner','public','CREATE')
       OR pg_catalog.pg_has_role('account_service','account_approval_authorization_owner','MEMBER') THEN
        RAISE EXCEPTION 'ordinary replay owner boundary is invalid' USING ERRCODE='42501';
    END IF;
END $$;

-- Only the independent migrator can SET ROLE to this existing owner. CREATE
-- on public is borrowed inside this migration transaction and returned below.
GRANT CREATE ON SCHEMA public TO account_approval_authorization_owner;
RESET ROLE;
SET ROLE account_approval_authorization_owner;

CREATE TABLE account_approval_authorization.ordinary_replay_generations (
    replay_id UUID PRIMARY KEY REFERENCES public.event_replay_requests(replay_id) ON DELETE RESTRICT,
    event_id UUID NOT NULL REFERENCES public.outbox_events(event_id) ON DELETE RESTRICT,
    event_digest TEXT NOT NULL CHECK (event_digest ~ '^[0-9a-f]{64}$'),
    generation_before BIGINT NOT NULL CHECK (generation_before >= 0),
    generation_after BIGINT NOT NULL CHECK (generation_after = generation_before + 1),
    enqueued_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    UNIQUE(event_id,generation_after)
);
CREATE FUNCTION account_approval_authorization.reject_ordinary_replay_generation_mutation()
RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$ BEGIN
    RAISE EXCEPTION 'ordinary replay generation evidence is immutable' USING ERRCODE='55000';
END $$;
CREATE TRIGGER ordinary_replay_generations_immutable
BEFORE UPDATE OR DELETE OR TRUNCATE ON account_approval_authorization.ordinary_replay_generations
FOR EACH STATEMENT EXECUTE FUNCTION account_approval_authorization.reject_ordinary_replay_generation_mutation();

CREATE FUNCTION public.enqueue_ordinary_dead_letter_replay(
    p_replay_id UUID,p_event_id UUID,p_expected_generation BIGINT,p_event_digest TEXT,
    p_idempotency_key TEXT,p_request_hash TEXT,p_service_name TEXT,p_job_type TEXT,
    p_job_run_id TEXT,p_reason TEXT,p_envelope BYTEA
) RETURNS TABLE(replay_id UUID,event_id UUID,replayed BOOLEAN)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE
    event_row public.outbox_events%ROWTYPE;
    receipt public.event_replay_requests%ROWTYPE;
    envelope JSONB;
    changed BIGINT;
BEGIN
    IF session_user <> 'account_service' THEN
        RAISE EXCEPTION 'Account runtime replay identity is required' USING ERRCODE='42501';
    END IF;
    IF p_event_id IS NULL OR p_event_id='00000000-0000-0000-0000-000000000000'::UUID
       OR p_event_digest IS NULL OR p_event_digest !~ '^[0-9a-f]{64}$'
       OR p_request_hash IS NULL OR p_request_hash !~ '^[0-9a-f]{64}$'
       OR p_idempotency_key IS NULL OR char_length(p_idempotency_key) NOT BETWEEN 16 AND 128
       OR p_reason IS NULL OR char_length(p_reason)<20
       OR p_service_name IS NULL OR p_service_name=''
       OR p_job_type IS DISTINCT FROM 'EVENT_REPLAY'
       OR p_job_run_id IS NULL OR p_job_run_id='' THEN
        RAISE EXCEPTION 'ordinary replay request facts are invalid' USING ERRCODE='23514';
    END IF;
    SELECT o.* INTO event_row FROM public.outbox_events o WHERE o.event_id=p_event_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'ordinary replay event is unavailable' USING ERRCODE='55000';
    END IF;
    -- This precedes every result-first path, including an old generic receipt.
    IF event_row.event_type='account.approval-command.authorized.v1' THEN
        RAISE EXCEPTION 'AUTHORIZATION_EVENT_REPLAY_FORBIDDEN' USING ERRCODE='55000';
    END IF;
    SELECT r.* INTO receipt FROM public.event_replay_requests r WHERE r.idempotency_key=p_idempotency_key;
    IF FOUND THEN
        IF receipt.event_id IS DISTINCT FROM p_event_id OR receipt.event_digest IS DISTINCT FROM p_event_digest
           OR receipt.request_hash IS DISTINCT FROM p_request_hash OR receipt.actor_type IS DISTINCT FROM 'SERVICE'
           OR receipt.service_name IS DISTINCT FROM p_service_name OR receipt.job_type IS DISTINCT FROM p_job_type
           OR receipt.reason IS DISTINCT FROM p_reason THEN
            RAISE EXCEPTION 'event replay idempotency key conflicts with another request' USING ERRCODE='23505';
        END IF;
        -- A receipt is a result, never authority to enqueue it again. In
        -- particular, this remains valid after publication or generation drift.
        RETURN QUERY SELECT receipt.replay_id,receipt.event_id,false;
        RETURN;
    END IF;
    IF p_replay_id IS NULL OR p_replay_id='00000000-0000-0000-0000-000000000000'::UUID
       OR event_row.published_at IS NOT NULL OR event_row.dead_lettered_at IS NULL
       OR p_expected_generation IS NULL OR p_expected_generation<0
       OR event_row.publication_generation IS DISTINCT FROM p_expected_generation
       OR p_envelope IS NULL OR pg_catalog.encode(pg_catalog.sha256(p_envelope),'hex') IS DISTINCT FROM p_event_digest THEN
        RAISE EXCEPTION 'ordinary dead-letter replay facts changed' USING ERRCODE='55000';
    END IF;
    envelope := pg_catalog.convert_from(p_envelope,'UTF8')::JSONB;
    IF pg_catalog.jsonb_typeof(envelope) IS DISTINCT FROM 'object'
       OR (SELECT count(*) FROM pg_catalog.jsonb_object_keys(envelope))<>7
       OR (envelope->>'event_id')::UUID IS DISTINCT FROM event_row.event_id
       OR envelope->>'event_type' IS DISTINCT FROM event_row.event_type
       OR (envelope->>'aggregate_id')::UUID IS DISTINCT FROM event_row.aggregate_id
       OR (envelope->>'aggregate_version')::BIGINT IS DISTINCT FROM event_row.aggregate_version
       OR (envelope->>'occurred_at')::TIMESTAMPTZ IS DISTINCT FROM event_row.occurred_at
       OR envelope->>'trace_id' IS DISTINCT FROM event_row.trace_id
       OR envelope->'payload' IS DISTINCT FROM event_row.payload THEN
        RAISE EXCEPTION 'ordinary replay immutable envelope does not match' USING ERRCODE='23514';
    END IF;
    INSERT INTO public.event_replay_requests(replay_id,event_id,event_digest,idempotency_key,request_hash,actor_type,service_name,job_type,job_run_id,reason,replayed_at)
    VALUES(p_replay_id,p_event_id,p_event_digest,p_idempotency_key,p_request_hash,'SERVICE',p_service_name,p_job_type,p_job_run_id,p_reason,transaction_timestamp());
    INSERT INTO account_approval_authorization.ordinary_replay_generations(replay_id,event_id,event_digest,generation_before,generation_after)
    VALUES(p_replay_id,p_event_id,p_event_digest,p_expected_generation,p_expected_generation+1);
    UPDATE public.outbox_events o SET dead_lettered_at=NULL,publication_generation=p_expected_generation+1,
        attempts=0,last_error='audited replay queued: '||p_replay_id::TEXT,next_attempt_at=transaction_timestamp(),locked_by=NULL,locked_until=NULL
    WHERE o.event_id=p_event_id AND o.event_type<>'account.approval-command.authorized.v1'
      AND o.published_at IS NULL AND o.dead_lettered_at IS NOT NULL AND o.publication_generation=p_expected_generation;
    GET DIAGNOSTICS changed=ROW_COUNT;
    IF changed<>1 THEN
        RAISE EXCEPTION 'dead-letter event changed before replay was committed' USING ERRCODE='55000';
    END IF;
    RETURN QUERY SELECT p_replay_id,p_event_id,true;
END $$;
REVOKE ALL ON TABLE account_approval_authorization.ordinary_replay_generations
FROM PUBLIC,account_service,account_approval_writer,account_approval_backfill,account_approval_relay,account_approval_recovery;
REVOKE ALL ON FUNCTION account_approval_authorization.reject_ordinary_replay_generation_mutation()
FROM PUBLIC,account_service,account_approval_writer,account_approval_backfill,account_approval_relay,account_approval_recovery;
REVOKE ALL ON FUNCTION public.enqueue_ordinary_dead_letter_replay(UUID,UUID,BIGINT,TEXT,TEXT,TEXT,TEXT,TEXT,TEXT,TEXT,BYTEA)
FROM PUBLIC,account_service,account_approval_writer,account_approval_backfill,account_approval_relay,account_approval_recovery;
GRANT EXECUTE ON FUNCTION public.enqueue_ordinary_dead_letter_replay(UUID,UUID,BIGINT,TEXT,TEXT,TEXT,TEXT,TEXT,TEXT,TEXT,BYTEA) TO account_service;
RESET ROLE;
SET ROLE account_service;
REVOKE CREATE ON SCHEMA public FROM account_approval_authorization_owner;
