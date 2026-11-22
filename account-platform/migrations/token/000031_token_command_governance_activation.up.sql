DO $$
BEGIN
    IF session_user<>'token_migrator' AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname=session_user AND rolsuper) THEN
        RAISE EXCEPTION '000031 requires the dedicated token_migrator identity';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname='token_schema_owner' AND NOT rolcanlogin)
       OR pg_catalog.pg_has_role('token_gateway','token_schema_owner','MEMBER')
       OR pg_catalog.pg_has_role('token_gateway',(SELECT datdba FROM pg_catalog.pg_database WHERE datname=current_database()),'MEMBER')
       OR (SELECT pg_catalog.pg_get_userbyid(nspowner) FROM pg_catalog.pg_namespace WHERE oid=pg_catalog.to_regnamespace('public')) IS DISTINCT FROM 'token_schema_owner' THEN
        RAISE EXCEPTION '000031 requires an isolated NOLOGIN schema owner';
    END IF;
END $$;
SET ROLE token_schema_owner;

CREATE TABLE public.token_command_governance_activation (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK(singleton),
    state TEXT NOT NULL CHECK(state IN ('PRE_ACTIVATION','ACTIVE','SAFE_ROLLBACK')),
    epoch BIGINT NOT NULL CHECK(epoch>=0),
    policy_set_digest TEXT,
    handler_set_digest TEXT,
    approved_runtime_roles TEXT[] NOT NULL DEFAULT '{}',
    operator_id TEXT,
    reason TEXT,
    request_id UUID,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    CHECK ((state='PRE_ACTIVATION' AND epoch=0 AND policy_set_digest IS NULL AND handler_set_digest IS NULL AND cardinality(approved_runtime_roles)=0 AND operator_id IS NULL AND reason IS NULL AND request_id IS NULL)
       OR (state<>'PRE_ACTIVATION' AND epoch>0 AND policy_set_digest IS NOT NULL AND policy_set_digest ~ '^[0-9a-f]{64}$' AND handler_set_digest IS NOT NULL AND handler_set_digest ~ '^[0-9a-f]{64}$' AND cardinality(approved_runtime_roles)>0 AND operator_id IS NOT NULL AND operator_id=btrim(operator_id) AND char_length(operator_id) BETWEEN 1 AND 128 AND reason IS NOT NULL AND reason=btrim(reason) AND char_length(reason) BETWEEN 1 AND 500 AND request_id IS NOT NULL))
);
INSERT INTO public.token_command_governance_activation(singleton,state,epoch) VALUES(TRUE,'PRE_ACTIVATION',0);

CREATE TABLE public.token_command_governance_transitions (
    epoch BIGINT PRIMARY KEY CHECK(epoch>0),
    previous_state TEXT NOT NULL,
    state TEXT NOT NULL,
    policy_set_digest TEXT NOT NULL,
    handler_set_digest TEXT NOT NULL,
    approved_runtime_roles TEXT[] NOT NULL,
    operator_id TEXT NOT NULL,
    reason TEXT NOT NULL,
    request_id UUID NOT NULL UNIQUE,
    changed_at TIMESTAMPTZ NOT NULL,
    CHECK ((epoch=1 AND previous_state='PRE_ACTIVATION' AND state='ACTIVE') OR (epoch>1 AND previous_state='ACTIVE' AND state='SAFE_ROLLBACK') OR (epoch>1 AND previous_state='SAFE_ROLLBACK' AND state='ACTIVE'))
);

CREATE FUNCTION public.protect_token_governance_transition_history() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
    RAISE EXCEPTION 'Token governance transition history is immutable' USING ERRCODE='55000';
END $$;
CREATE TRIGGER token_governance_transition_history_immutable BEFORE UPDATE OR DELETE ON public.token_command_governance_transitions
FOR EACH ROW EXECUTE FUNCTION public.protect_token_governance_transition_history();

CREATE FUNCTION public.protect_token_governance_activation() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
    IF TG_OP='DELETE' OR NEW.singleton IS DISTINCT FROM OLD.singleton
       OR NEW.epoch IS DISTINCT FROM OLD.epoch+1
       OR NOT ((OLD.state='PRE_ACTIVATION' AND OLD.epoch=0 AND NEW.state='ACTIVE') OR (OLD.state='ACTIVE' AND NEW.state='SAFE_ROLLBACK') OR (OLD.state='SAFE_ROLLBACK' AND NEW.state='ACTIVE')) THEN
        RAISE EXCEPTION 'invalid Token governance activation transition' USING ERRCODE='23514';
    END IF;
    IF NEW.state='SAFE_ROLLBACK' AND (NEW.policy_set_digest IS DISTINCT FROM OLD.policy_set_digest OR NEW.handler_set_digest IS DISTINCT FROM OLD.handler_set_digest OR NEW.approved_runtime_roles IS DISTINCT FROM OLD.approved_runtime_roles) THEN
        RAISE EXCEPTION 'SAFE transition must retain the selected release evidence' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER token_governance_activation_guard BEFORE UPDATE OR DELETE ON public.token_command_governance_activation
FOR EACH ROW EXECUTE FUNCTION public.protect_token_governance_activation();

CREATE FUNCTION public.record_token_governance_transition() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
    INSERT INTO public.token_command_governance_transitions(epoch,previous_state,state,policy_set_digest,handler_set_digest,approved_runtime_roles,operator_id,reason,request_id,changed_at)
    VALUES(NEW.epoch,OLD.state,NEW.state,NEW.policy_set_digest,NEW.handler_set_digest,NEW.approved_runtime_roles,NEW.operator_id,NEW.reason,NEW.request_id,NEW.updated_at);
    RETURN NEW;
END $$;
CREATE TRIGGER token_governance_activation_audit AFTER UPDATE ON public.token_command_governance_activation
FOR EACH ROW EXECUTE FUNCTION public.record_token_governance_transition();

REVOKE ALL ON public.token_command_governance_activation,public.token_command_governance_transitions FROM PUBLIC,token_gateway;
GRANT SELECT ON public.token_command_governance_activation,public.token_command_governance_transitions TO token_gateway;
-- FOR SHARE requires UPDATE privilege when issued directly. Expose only the
-- fixed read-and-lock operation, never grant runtime activation UPDATE.
CREATE FUNCTION public.lock_token_command_governance_activation()
RETURNS SETOF public.token_command_governance_activation
LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog AS $$
    SELECT * FROM public.token_command_governance_activation WHERE singleton=TRUE FOR SHARE
$$;
REVOKE ALL ON FUNCTION public.lock_token_command_governance_activation() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.lock_token_command_governance_activation() TO token_gateway;
REVOKE ALL ON FUNCTION public.protect_token_governance_transition_history(),public.protect_token_governance_activation(),public.record_token_governance_transition() FROM PUBLIC;
SET ROLE token_gateway;
