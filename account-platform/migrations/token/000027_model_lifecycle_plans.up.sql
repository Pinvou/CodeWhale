CREATE TABLE model_lifecycle_plans (
    id UUID PRIMARY KEY,
    model_id UUID NOT NULL REFERENCES model_identities(id),
    plan_version BIGINT NOT NULL CHECK (plan_version > 0),
    expected_catalog_version BIGINT NOT NULL REFERENCES model_catalog_versions(version),
    current_lifecycle TEXT NOT NULL CHECK (current_lifecycle IN ('DRAFT','PUBLISHED','DEPRECATED')),
    target_lifecycle TEXT NOT NULL CHECK (target_lifecycle IN ('PUBLISHED','DEPRECATED','REMOVED')),
    effective_at TIMESTAMPTZ NOT NULL,
    existing_session_cutoff_at TIMESTAMPTZ,
    replacement_model_id UUID REFERENCES model_identities(id),
    force_cancel_inflight BOOLEAN NOT NULL DEFAULT false,
    status TEXT NOT NULL CHECK (status IN ('PENDING_APPROVAL','SCHEDULED','EXECUTING','APPLIED','CANCELLED','EXPIRED','INVALIDATED','FAILED','UNKNOWN')),
    reason TEXT NOT NULL CHECK (length(reason) BETWEEN 10 AND 2000),
    request_digest TEXT NOT NULL CHECK (request_digest ~ '^[0-9a-f]{64}$'),
    idempotency_key_digest TEXT NOT NULL CHECK (idempotency_key_digest ~ '^[0-9a-f]{64}$'),
    cancel_request_digest TEXT CHECK (cancel_request_digest IS NULL OR cancel_request_digest ~ '^[0-9a-f]{64}$'),
    cancel_idempotency_key_digest TEXT CHECK (cancel_idempotency_key_digest IS NULL OR cancel_idempotency_key_digest ~ '^[0-9a-f]{64}$'),
    cancel_actor_id UUID,
    approval_request_id UUID UNIQUE,
    approval_command_fingerprint TEXT CHECK (approval_command_fingerprint IS NULL OR approval_command_fingerprint ~ '^[0-9a-f]{64}$'),
    governed_command_reference UUID UNIQUE REFERENCES token_command_results(command_reference),
    critical_resource_versions JSONB,
    financial_snapshot JSONB,
    execution_lease_owner TEXT,
    execution_lease_until TIMESTAMPTZ,
    execution_attempt INTEGER NOT NULL DEFAULT 0 CHECK (execution_attempt >= 0),
    result_catalog_version BIGINT REFERENCES model_catalog_versions(version),
    stable_error_code TEXT CHECK (stable_error_code IS NULL OR stable_error_code ~ '^[A-Z][A-Z0-9_]{0,63}$'),
    created_by UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    cancelled_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    UNIQUE(model_id,plan_version),
    UNIQUE(created_by,idempotency_key_digest),
    CHECK (replacement_model_id IS NULL OR replacement_model_id<>model_id),
    CONSTRAINT model_lifecycle_plans_transition_ck CHECK ((current_lifecycle='DRAFT' AND target_lifecycle='PUBLISHED') OR (current_lifecycle='PUBLISHED' AND target_lifecycle IN ('DEPRECATED','REMOVED')) OR (current_lifecycle='DEPRECATED' AND target_lifecycle='REMOVED')),
    CHECK ((target_lifecycle='REMOVED')=(replacement_model_id IS NOT NULL)),
    CHECK ((target_lifecycle='REMOVED' AND existing_session_cutoff_at IS NOT DISTINCT FROM effective_at) OR (target_lifecycle<>'REMOVED' AND (existing_session_cutoff_at IS NULL OR existing_session_cutoff_at>=effective_at))),
    CHECK (target_lifecycle<>'PUBLISHED' OR (replacement_model_id IS NULL AND existing_session_cutoff_at IS NULL)),
    CHECK ((execution_lease_owner IS NULL AND execution_lease_until IS NULL) OR (execution_lease_owner IS NOT NULL AND execution_lease_until IS NOT NULL)),
    CHECK ((status='APPLIED')=(result_catalog_version IS NOT NULL)),
    CHECK ((approval_request_id IS NULL)=(approval_command_fingerprint IS NULL)),
    CHECK ((target_lifecycle='PUBLISHED')=(governed_command_reference IS NOT NULL)),
    CHECK (target_lifecycle='PUBLISHED' OR governed_command_reference IS NULL),
    CHECK (target_lifecycle<>'PUBLISHED' OR (approval_request_id IS NULL AND approval_command_fingerprint IS NULL)),
    CHECK ((cancel_request_digest IS NULL)=(cancel_idempotency_key_digest IS NULL) AND (cancel_request_digest IS NULL)=(cancel_actor_id IS NULL)),
    CHECK ((critical_resource_versions IS NULL AND financial_snapshot IS NULL) OR (critical_resource_versions IS NOT NULL AND financial_snapshot IS NOT NULL))
);

CREATE UNIQUE INDEX model_lifecycle_plans_active_idx
    ON model_lifecycle_plans(model_id)
    WHERE status IN ('PENDING_APPROVAL','SCHEDULED','EXECUTING','UNKNOWN');

CREATE INDEX model_lifecycle_plans_model_history_idx
    ON model_lifecycle_plans(model_id,plan_version DESC,id DESC);

CREATE INDEX model_lifecycle_plans_due_idx
    ON model_lifecycle_plans(effective_at,id)
    WHERE status='SCHEDULED';

CREATE OR REPLACE FUNCTION preserve_model_lifecycle_plan_definition() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.target_lifecycle='PUBLISHED' THEN
        IF NEW.governed_command_reference IS NULL
           OR NEW.approval_request_id IS NOT NULL
           OR NEW.approval_command_fingerprint IS NOT NULL
           OR NEW.financial_snapshot IS DISTINCT FROM '{}'::jsonb
           OR jsonb_typeof(NEW.critical_resource_versions)<>'object'
           OR (SELECT count(*) FROM jsonb_object_keys(NEW.critical_resource_versions))<>15
           OR NOT NEW.critical_resource_versions ?& ARRAY['model_id','expected_catalog_version','expected_draft_version','draft_fingerprint','protocol_definition_id','protocol_version','stable_route_count','stable_route_set_fingerprint','provisioning_count','ready_route_count','provisioning_set_fingerprint','financial_fact_revision','price_set_fingerprint','precheck_fingerprint','request_fingerprint']
           OR (NEW.critical_resource_versions->>'model_id')::uuid<>NEW.model_id
           OR (NEW.critical_resource_versions->>'expected_catalog_version')::bigint<>NEW.expected_catalog_version
           OR (NEW.critical_resource_versions->>'expected_draft_version')::bigint<1
           OR (NEW.critical_resource_versions->>'protocol_definition_id')::uuid IS NULL
           OR (NEW.critical_resource_versions->>'protocol_version')::bigint<1
           OR (NEW.critical_resource_versions->>'stable_route_count')::bigint<1
           OR (NEW.critical_resource_versions->>'provisioning_count')::bigint<1
           OR (NEW.critical_resource_versions->>'ready_route_count')::bigint<(NEW.critical_resource_versions->>'stable_route_count')::bigint
           OR (NEW.critical_resource_versions->>'financial_fact_revision')::bigint<1
           OR NEW.critical_resource_versions->>'draft_fingerprint' !~ '^[0-9a-f]{64}$'
           OR NEW.critical_resource_versions->>'stable_route_set_fingerprint' !~ '^[0-9a-f]{64}$'
           OR NEW.critical_resource_versions->>'provisioning_set_fingerprint' !~ '^[0-9a-f]{64}$'
           OR NEW.critical_resource_versions->>'price_set_fingerprint' !~ '^[0-9a-f]{64}$'
           OR NEW.critical_resource_versions->>'precheck_fingerprint' !~ '^[0-9a-f]{64}$'
           OR NEW.critical_resource_versions->>'request_fingerprint' !~ '^[0-9a-f]{64}$'
           OR NOT EXISTS (
               SELECT 1 FROM token_command_results command
               WHERE command.command_reference=NEW.governed_command_reference
                 AND command.command_type='TOKEN_MODEL_PUBLISH'
                 AND command.actor_user_id=NEW.created_by
                 AND command.target_resource_type='MODEL'
                 AND command.target_resource_id=NEW.model_id::text
                 AND command.expected_versions=NEW.critical_resource_versions
                 AND command.status IN ('PENDING','SUCCEEDED')
                 AND (TG_OP<>'INSERT' OR command.status='PENDING')
           ) THEN
            RAISE EXCEPTION 'scheduled model publication requires an exact frozen governed snapshot';
        END IF;
    ELSIF TG_OP='INSERT' AND (NEW.approval_request_id IS NOT NULL OR NEW.approval_command_fingerprint IS NOT NULL OR NEW.critical_resource_versions IS NOT NULL OR NEW.financial_snapshot IS NOT NULL) THEN
        RAISE EXCEPTION 'lifecycle approval snapshot must be frozen after plan creation';
    END IF;
    IF TG_OP='INSERT' THEN
        RETURN NEW;
    END IF;
    IF ROW(NEW.model_id,NEW.plan_version,NEW.expected_catalog_version,NEW.current_lifecycle,NEW.target_lifecycle,NEW.effective_at,NEW.existing_session_cutoff_at,NEW.replacement_model_id,NEW.force_cancel_inflight,NEW.reason,NEW.request_digest,NEW.idempotency_key_digest,NEW.governed_command_reference,NEW.created_by,NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.model_id,OLD.plan_version,OLD.expected_catalog_version,OLD.current_lifecycle,OLD.target_lifecycle,OLD.effective_at,OLD.existing_session_cutoff_at,OLD.replacement_model_id,OLD.force_cancel_inflight,OLD.reason,OLD.request_digest,OLD.idempotency_key_digest,OLD.governed_command_reference,OLD.created_by,OLD.created_at) THEN
        RAISE EXCEPTION 'model lifecycle plan definitions are immutable';
    END IF;
    IF ROW(NEW.approval_request_id,NEW.approval_command_fingerprint,NEW.critical_resource_versions,NEW.financial_snapshot)
       IS DISTINCT FROM
       ROW(OLD.approval_request_id,OLD.approval_command_fingerprint,OLD.critical_resource_versions,OLD.financial_snapshot) THEN
        IF OLD.target_lifecycle='PUBLISHED'
           OR OLD.status<>'PENDING_APPROVAL'
           OR NEW.status<>'SCHEDULED'
           OR OLD.approval_request_id IS NOT NULL
           OR OLD.approval_command_fingerprint IS NOT NULL
           OR OLD.critical_resource_versions IS NOT NULL
           OR OLD.financial_snapshot IS NOT NULL
           OR NEW.approval_request_id IS NULL
           OR NEW.approval_command_fingerprint IS NULL
           OR NEW.critical_resource_versions IS NULL
           OR NEW.financial_snapshot IS NULL THEN
            RAISE EXCEPTION 'model lifecycle approval snapshot is immutable after one freeze';
        END IF;
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER model_lifecycle_plan_definition_immutable
    BEFORE INSERT OR UPDATE ON model_lifecycle_plans
    FOR EACH ROW EXECUTE FUNCTION preserve_model_lifecycle_plan_definition();

CREATE TABLE model_lifecycle_plan_events (
    id UUID PRIMARY KEY,
    plan_id UUID NOT NULL REFERENCES model_lifecycle_plans(id),
    plan_version BIGINT NOT NULL CHECK (plan_version > 0),
    event_type TEXT NOT NULL CHECK (event_type IN ('CREATED','RESCHEDULED','APPROVED','LEASED','APPLIED','CANCELLED','EXPIRED','INVALIDATED','FAILED','UNKNOWN')),
    actor_type TEXT NOT NULL CHECK (actor_type IN ('USER','SERVICE')),
    actor_id TEXT NOT NULL,
    reason TEXT NOT NULL,
    stable_error_code TEXT CHECK (stable_error_code IS NULL OR stable_error_code ~ '^[A-Z][A-Z0-9_]{0,63}$'),
    evidence JSONB NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX model_lifecycle_plan_events_plan_idx
    ON model_lifecycle_plan_events(plan_id,occurred_at DESC,id DESC);

CREATE OR REPLACE FUNCTION reject_model_lifecycle_plan_event_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'model lifecycle plan events are immutable';
END $$;

CREATE TRIGGER model_lifecycle_plan_event_immutable
    BEFORE UPDATE OR DELETE ON model_lifecycle_plan_events
    FOR EACH ROW EXECUTE FUNCTION reject_model_lifecycle_plan_event_mutation();
