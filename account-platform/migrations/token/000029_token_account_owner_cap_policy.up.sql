DO $$
DECLARE is_superuser BOOLEAN;
BEGIN
    SELECT rolsuper INTO is_superuser FROM pg_catalog.pg_roles WHERE rolname=session_user;
    IF session_user<>'token_migrator' AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname=session_user AND rolsuper) THEN
        RAISE EXCEPTION '000029 requires the dedicated token_migrator deployment identity';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname='token_gateway')
       OR NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname='token_cap_policy_deployer')
	   OR NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname='token_schema_owner' AND NOT rolcanlogin)
       OR NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname='token_migrator' AND rolcanlogin AND NOT rolinherit)
       OR NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname='token_policy_operator' AND rolcanlogin AND NOT rolinherit) THEN
        RAISE EXCEPTION '000029 deployment roles must be provisioned before migration';
    END IF;
    IF session_user='token_migrator' AND NOT pg_catalog.pg_has_role(session_user,'token_gateway','MEMBER') THEN
        RAISE EXCEPTION 'token_migrator must be a member of token_gateway';
    END IF;
    IF session_user='token_migrator' AND NOT pg_catalog.pg_has_role(session_user,'token_cap_policy_deployer','MEMBER') THEN
        RAISE EXCEPTION 'token_migrator must be a member of token_cap_policy_deployer';
    END IF;
    IF NOT is_superuser AND current_user<>'token_gateway' THEN
        RAISE EXCEPTION '000029 must run with MIGRATION_ROLE=token_gateway';
    END IF;
    IF session_user='token_migrator' AND NOT pg_catalog.pg_has_role(session_user,'token_schema_owner','MEMBER') THEN
        RAISE EXCEPTION 'token_migrator must be a member of token_schema_owner';
    END IF;
    IF pg_catalog.pg_has_role('token_gateway','token_schema_owner','MEMBER') THEN
        RAISE EXCEPTION 'runtime must not be a member of token_schema_owner';
    END IF;
    IF pg_catalog.pg_has_role('token_gateway',(SELECT datdba FROM pg_catalog.pg_database WHERE datname=current_database()),'MEMBER') THEN
        RAISE EXCEPTION 'token runtime must not own or inherit the database owner';
    END IF;
    IF (SELECT pg_catalog.pg_get_userbyid(nspowner) FROM pg_catalog.pg_namespace WHERE oid=pg_catalog.to_regnamespace('public')) IS DISTINCT FROM 'token_schema_owner' THEN
        RAISE EXCEPTION 'public schema must be owned by token_schema_owner';
    END IF;
    EXECUTE 'GRANT REFERENCES ON public.token_command_results TO token_cap_policy_deployer';
    EXECUTE 'GRANT INSERT ON public.outbox_events TO token_cap_policy_deployer';
END $$;

SET ROLE token_schema_owner;
GRANT USAGE,CREATE ON SCHEMA public TO token_cap_policy_deployer;
REVOKE CREATE ON SCHEMA public FROM token_gateway,PUBLIC;
SET ROLE token_cap_policy_deployer;

CREATE TABLE public.token_account_owner_cap_policy_revisions (
    policy_version BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY CHECK (policy_version>=1),
    max_per_owner INTEGER NOT NULL CHECK (max_per_owner BETWEEN 1 AND 1000),
    config_digest CHAR(64) NOT NULL CHECK (config_digest ~ '^[0-9a-f]{64}$'),
    request_digest CHAR(64) NOT NULL CHECK (request_digest ~ '^[0-9a-f]{64}$'),
    idempotency_key_digest CHAR(64) NOT NULL CHECK (idempotency_key_digest ~ '^[0-9a-f]{64}$'),
    principal_digest CHAR(64) NOT NULL CHECK (principal_digest ~ '^[0-9a-f]{64}$'),
    changed_by VARCHAR(128) NOT NULL CHECK (changed_by<>''),
    reason TEXT NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 500 AND reason=btrim(reason)),
    request_id UUID NOT NULL, outbox_event_id UUID NOT NULL UNIQUE,
    changed_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    UNIQUE(principal_digest,idempotency_key_digest)
);
CREATE TABLE public.token_account_owner_cap_policy_active (
    singleton_key BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK(singleton_key),
    policy_id UUID NOT NULL UNIQUE,
    active_policy_version BIGINT REFERENCES public.token_account_owner_cap_policy_revisions(policy_version),
    pointer_version BIGINT NOT NULL DEFAULT 0 CHECK(pointer_version>=0),
    updated_by VARCHAR(128), updated_at TIMESTAMPTZ,
    CHECK ((active_policy_version IS NULL AND pointer_version=0 AND updated_by IS NULL AND updated_at IS NULL) OR
           (active_policy_version IS NOT NULL AND pointer_version=active_policy_version AND updated_by IS NOT NULL AND updated_at IS NOT NULL))
);
INSERT INTO public.token_account_owner_cap_policy_active(singleton_key,policy_id)
VALUES(TRUE,'00000000-0000-7000-8000-000000000029');

CREATE TABLE public.token_account_provisioning_reservations (
    command_reference UUID PRIMARY KEY REFERENCES public.token_command_results(command_reference),
    actor_user_id UUID NOT NULL,
    owner_type TEXT NOT NULL CHECK(owner_type IN ('USER','ORG','ORG_UNIT')), owner_id UUID NOT NULL,
    owner_organization_id UUID, owner_unit_id UUID,
    account_name TEXT NOT NULL CHECK(char_length(account_name) BETWEEN 1 AND 120),
    share_scope TEXT NOT NULL, default_priority INTEGER NOT NULL CHECK(default_priority BETWEEN 0 AND 10000),
    canonical_request_digest CHAR(64) NOT NULL CHECK(canonical_request_digest ~ '^[0-9a-f]{64}$'),
    initial_platform_authorization_version BIGINT NOT NULL CHECK(initial_platform_authorization_version>0),
	initial_actor_version BIGINT NOT NULL CHECK(initial_actor_version>0),
	initial_actor_identity_digest CHAR(64) NOT NULL CHECK(initial_actor_identity_digest ~ '^[0-9a-f]{64}$'),
    initial_owner_version BIGINT NOT NULL CHECK(initial_owner_version>0),
    initial_owner_identity_digest CHAR(64) NOT NULL CHECK(initial_owner_identity_digest ~ '^[0-9a-f]{64}$'),
    active_platform_authorization_version BIGINT NOT NULL CHECK(active_platform_authorization_version>=initial_platform_authorization_version),
	active_actor_version BIGINT NOT NULL CHECK(active_actor_version>=initial_actor_version),
	active_actor_identity_digest CHAR(64) NOT NULL CHECK(active_actor_identity_digest ~ '^[0-9a-f]{64}$'),
    active_owner_version BIGINT NOT NULL CHECK(active_owner_version>=initial_owner_version),
    active_owner_identity_digest CHAR(64) NOT NULL CHECK(active_owner_identity_digest ~ '^[0-9a-f]{64}$'),
    policy_version BIGINT NOT NULL REFERENCES public.token_account_owner_cap_policy_revisions(policy_version),
    activation_epoch BIGINT NOT NULL CHECK(activation_epoch>0), recovery_epoch BIGINT NOT NULL DEFAULT 1 CHECK(recovery_epoch>0),
    reservation_version BIGINT NOT NULL DEFAULT 1 CHECK(reservation_version>0),
    status TEXT NOT NULL CHECK(status IN ('PENDING','MANUAL_REVIEW','COMPLETED','FAILED')),
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK(attempt_count BETWEEN 0 AND 8),
    next_attempt_at TIMESTAMPTZ NOT NULL, expires_at TIMESTAMPTZ NOT NULL,
    lease_owner TEXT, lease_until TIMESTAMPTZ, lease_fencing_token UUID,
    token_account_id UUID UNIQUE, stable_error_code TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(), updated_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    CHECK(expires_at>created_at),
    CHECK((lease_owner IS NULL)=(lease_until IS NULL) AND (lease_owner IS NULL)=(lease_fencing_token IS NULL)),
    CHECK((status='COMPLETED')=(token_account_id IS NOT NULL)),
    CHECK(stable_error_code IS NULL OR (stable_error_code ~ '^[A-Z][A-Z0-9_]{0,119}$' AND stable_error_code !~ '(TOKEN|KEY|SECRET|PASSWORD|CREDENTIAL)'))
);
CREATE INDEX token_account_provisioning_reservations_lease_idx ON public.token_account_provisioning_reservations(next_attempt_at,created_at,command_reference) WHERE status='PENDING';

CREATE TABLE public.token_account_provisioning_recovery_actions (
    action_id UUID PRIMARY KEY, command_reference UUID NOT NULL REFERENCES public.token_account_provisioning_reservations(command_reference),
    creator_actor_id UUID NOT NULL, operator_actor_id UUID NOT NULL,
	expected_operator_platform_authorization_version BIGINT NOT NULL CHECK(expected_operator_platform_authorization_version>0),
	expected_operator_actor_version BIGINT NOT NULL CHECK(expected_operator_actor_version>0),
	expected_operator_actor_identity_digest CHAR(64) NOT NULL CHECK(expected_operator_actor_identity_digest ~ '^[0-9a-f]{64}$'),
	expected_operator_owner_version BIGINT NOT NULL CHECK(expected_operator_owner_version>0),
	expected_operator_owner_identity_digest CHAR(64) NOT NULL CHECK(expected_operator_owner_identity_digest ~ '^[0-9a-f]{64}$'),
	operator_platform_authorization_version BIGINT NOT NULL CHECK(operator_platform_authorization_version>0),
	operator_actor_version BIGINT NOT NULL CHECK(operator_actor_version>0),
	operator_actor_identity_digest CHAR(64) NOT NULL CHECK(operator_actor_identity_digest ~ '^[0-9a-f]{64}$'),
	operator_owner_version BIGINT NOT NULL CHECK(operator_owner_version>0),
	operator_owner_identity_digest CHAR(64) NOT NULL CHECK(operator_owner_identity_digest ~ '^[0-9a-f]{64}$'),
    action TEXT NOT NULL CHECK(action IN ('REQUEUE','CANCEL')),
    key_digest CHAR(64) NOT NULL CHECK(key_digest ~ '^[0-9a-f]{64}$'), request_digest CHAR(64) NOT NULL CHECK(request_digest ~ '^[0-9a-f]{64}$'),
    expected_recovery_epoch BIGINT NOT NULL CHECK(expected_recovery_epoch>0), status TEXT NOT NULL CHECK(status IN ('PENDING','SUCCEEDED','FAILED')),
    stable_error_code TEXT, result_recovery_epoch BIGINT, audit_reference UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(), completed_at TIMESTAMPTZ,
    UNIQUE(operator_actor_id,command_reference,key_digest),
    CHECK((status='PENDING')=(completed_at IS NULL)), CHECK((status='SUCCEEDED')=(result_recovery_epoch IS NOT NULL)),
    CHECK((status='FAILED')=(stable_error_code IS NOT NULL)),
    CHECK(stable_error_code IS NULL OR (stable_error_code ~ '^[A-Z][A-Z0-9_]{0,119}$' AND stable_error_code !~ '(TOKEN|KEY|SECRET|PASSWORD|CREDENTIAL)'))
);

CREATE FUNCTION public.reject_token_account_owner_cap_policy_revision_mutation() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog AS $$ BEGIN RAISE EXCEPTION USING ERRCODE='55000',MESSAGE='TOKEN_ACCOUNT_OWNER_CAP_POLICY_REVISION_IMMUTABLE'; END $$;
CREATE TRIGGER token_account_owner_cap_policy_revision_immutable BEFORE UPDATE OR DELETE ON public.token_account_owner_cap_policy_revisions
FOR EACH ROW EXECUTE FUNCTION public.reject_token_account_owner_cap_policy_revision_mutation();

CREATE FUNCTION public.apply_token_account_owner_cap_policy(INTEGER,BIGINT,TEXT,TEXT,UUID)
RETURNS TABLE(policy_version BIGINT,max_per_owner INTEGER,config_digest TEXT,request_digest TEXT,outbox_event_id UUID,changed_by TEXT,changed_at TIMESTAMPTZ)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE requested_max ALIAS FOR $1; expected_version ALIAS FOR $2; idem ALIAS FOR $3; why ALIAS FOR $4; request_id ALIAS FOR $5;
 key_hash TEXT; actor_hash TEXT; input_hash TEXT; current_version BIGINT; next_version BIGINT; aggregate_id UUID; config_hash TEXT; event_id UUID;
 existing public.token_account_owner_cap_policy_revisions%ROWTYPE;
BEGIN
 IF session_user<>'token_policy_operator' THEN RAISE EXCEPTION USING ERRCODE='42501',MESSAGE='TOKEN_ACCOUNT_POLICY_OPERATOR_REQUIRED'; END IF;
 IF requested_max IS NULL OR requested_max NOT BETWEEN 1 AND 1000 OR idem IS NULL OR octet_length(idem) NOT BETWEEN 16 AND 128 OR
    why IS NULL OR btrim(why)<>why OR NOT(why IS NFC NORMALIZED) OR char_length(why) NOT BETWEEN 1 AND 500 OR request_id IS NULL OR request_id='00000000-0000-0000-0000-000000000000'::uuid
 THEN RAISE EXCEPTION USING ERRCODE='22023',MESSAGE='TOKEN_ACCOUNT_POLICY_CONFIG_MISMATCH'; END IF;
 key_hash:=pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to(idem,'UTF8')),'hex');
 actor_hash:=pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to(session_user,'UTF8')),'hex');
 input_hash:=pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to('max_per_owner='||requested_max::text||E'\nexpected_active_policy_version='||COALESCE(expected_version::text,'null')||E'\nreason='||pg_catalog.to_json(why)::text||E'\n','UTF8')),'hex');
 SELECT p.active_policy_version,p.policy_id INTO current_version,aggregate_id FROM public.token_account_owner_cap_policy_active p WHERE p.singleton_key FOR UPDATE;
 SELECT r.* INTO existing FROM public.token_account_owner_cap_policy_revisions r WHERE r.principal_digest=actor_hash AND r.idempotency_key_digest=key_hash;
 IF FOUND THEN
   IF existing.request_digest<>input_hash THEN RAISE EXCEPTION USING ERRCODE='23505',MESSAGE='IDEMPOTENCY_KEY_REUSED'; END IF;
   RETURN QUERY SELECT existing.policy_version,existing.max_per_owner,existing.config_digest::text,existing.request_digest::text,existing.outbox_event_id,existing.changed_by::text,existing.changed_at; RETURN;
 END IF;
 IF current_version IS DISTINCT FROM expected_version THEN RAISE EXCEPTION USING ERRCODE='40001',MESSAGE='COMMAND_PRECHECK_CHANGED'; END IF;
 SELECT COALESCE(max(r.policy_version),0)+1 INTO next_version FROM public.token_account_owner_cap_policy_revisions r;
 config_hash:=pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to('{"max_per_owner":'||requested_max::text||',"policy_version":'||next_version::text||'}','UTF8')),'hex'); event_id:=pg_catalog.gen_random_uuid();
 INSERT INTO public.token_account_owner_cap_policy_revisions(policy_version,max_per_owner,config_digest,request_digest,idempotency_key_digest,principal_digest,changed_by,reason,request_id,outbox_event_id)
 OVERRIDING SYSTEM VALUE VALUES(next_version,requested_max,config_hash,input_hash,key_hash,actor_hash,session_user,why,request_id,event_id) RETURNING * INTO existing;
 INSERT INTO public.outbox_events(event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at)
 VALUES(event_id,'token.account-owner-cap-policy.changed.v1',aggregate_id,next_version,pg_catalog.jsonb_build_object('policy_version',next_version,'max_per_owner',requested_max,'config_digest',config_hash),request_id::text,transaction_timestamp());
 UPDATE public.token_account_owner_cap_policy_active SET active_policy_version=next_version,pointer_version=next_version,updated_by=session_user,updated_at=transaction_timestamp() WHERE singleton_key;
 RETURN QUERY SELECT existing.policy_version,existing.max_per_owner,existing.config_digest::text,existing.request_digest::text,existing.outbox_event_id,existing.changed_by::text,existing.changed_at;
END $$;

REVOKE ALL ON FUNCTION public.apply_token_account_owner_cap_policy(INTEGER,BIGINT,TEXT,TEXT,UUID) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.apply_token_account_owner_cap_policy(INTEGER,BIGINT,TEXT,TEXT,UUID) TO token_policy_operator;
GRANT SELECT ON public.token_account_owner_cap_policy_revisions,public.token_account_owner_cap_policy_active TO token_gateway;
GRANT SELECT,INSERT,UPDATE ON public.token_account_provisioning_reservations,public.token_account_provisioning_recovery_actions TO token_gateway;
REVOKE INSERT,UPDATE,DELETE,TRUNCATE ON public.token_account_owner_cap_policy_revisions,public.token_account_owner_cap_policy_active FROM token_gateway,PUBLIC;
SET ROLE token_schema_owner;
REVOKE CREATE ON SCHEMA public FROM token_cap_policy_deployer;
SET ROLE token_gateway;
