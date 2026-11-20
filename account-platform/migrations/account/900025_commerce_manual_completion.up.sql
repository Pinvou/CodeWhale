DO $$ BEGIN
    IF session_user <> 'account_migrator' OR current_user <> 'account_service' THEN
        RAISE EXCEPTION 'commerce manual completion requires the Account migrator' USING ERRCODE='42501';
    END IF;
END $$;

-- These are local completion receipts, never supplier dispatch authorizations.
ALTER TABLE public.commerce_customer_sync_jobs DROP CONSTRAINT commerce_customer_sync_jobs_status_check;
ALTER TABLE public.commerce_customer_sync_jobs ADD CONSTRAINT commerce_customer_sync_jobs_status_check
    CHECK (status IN ('PENDING','LEASED','RETRY','COMPLETED','REPAIR_REQUIRED','CANCELLED_ACCOUNT_CLOSURE','MANUAL_NO_EFFECT'));

CREATE TABLE public.commerce_customer_repair_cases (
    id UUID PRIMARY KEY REFERENCES public.manual_review_cases(id),
    job_id UUID NOT NULL UNIQUE REFERENCES public.commerce_customer_sync_jobs(id),
    mapping_id UUID NOT NULL REFERENCES public.commerce_customer_mappings(id),
    mapping_version BIGINT NOT NULL CHECK (mapping_version>0),
    subject_type TEXT NOT NULL CHECK (subject_type IN ('USER','ORGANIZATION')),
    subject_id UUID NOT NULL,
    subject_lifecycle_version BIGINT NOT NULL CHECK (subject_lifecycle_version>0),
    provider_id TEXT NOT NULL,
    adapter_version TEXT NOT NULL,
    task_category TEXT NOT NULL CHECK (task_category IN ('CUSTOMER_PROVISIONING','PROFILE_SYNC')),
    attempts INTEGER NOT NULL CHECK (attempts>=0),
    version BIGINT NOT NULL DEFAULT 1 CHECK (version>0),
    status TEXT NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN','RESOLVED')),
    resolution TEXT CHECK (resolution IN ('SUCCEEDED_CURRENT','SUCCEEDED_EVIDENCE_ONLY','ENDED_NO_EFFECT')),
    approved_observation_id UUID,
    reviewer_user_id UUID,
    reviewer_binding_id UUID,
    reviewer_authorization_version BIGINT CHECK (reviewer_authorization_version>0),
    created_at TIMESTAMPTZ NOT NULL,
    resolved_at TIMESTAMPTZ,
    CHECK ((status='OPEN' AND resolution IS NULL AND approved_observation_id IS NULL AND reviewer_user_id IS NULL AND reviewer_binding_id IS NULL AND reviewer_authorization_version IS NULL AND resolved_at IS NULL)
        OR (status='RESOLVED' AND resolution IS NOT NULL AND approved_observation_id IS NOT NULL AND reviewer_user_id IS NOT NULL AND reviewer_binding_id IS NOT NULL AND reviewer_authorization_version IS NOT NULL AND resolved_at IS NOT NULL))
);
CREATE INDEX commerce_customer_repair_open_idx ON public.commerce_customer_repair_cases(id) WHERE status='OPEN';
CREATE INDEX commerce_customer_repair_mapping_idx ON public.commerce_customer_repair_cases(mapping_id,mapping_version);
ALTER TABLE public.commerce_customer_sync_jobs ADD COLUMN commerce_repair_case_id UUID REFERENCES public.commerce_customer_repair_cases(id);
CREATE INDEX commerce_customer_sync_manual_candidates_idx ON public.commerce_customer_sync_jobs(updated_at,id)
    WHERE commerce_repair_case_id IS NULL AND task_category IN ('CUSTOMER_PROVISIONING','PROFILE_SYNC') AND status IN ('REPAIR_REQUIRED','LEASED');

CREATE TABLE public.commerce_customer_repair_observations (
    id UUID PRIMARY KEY,
    case_id UUID NOT NULL REFERENCES public.commerce_customer_repair_cases(id),
    case_version BIGINT NOT NULL CHECK (case_version>1),
    outcome TEXT NOT NULL CHECK (outcome IN ('UNCONFIRMED','SUCCEEDED','ENDED_NO_EFFECT')),
    evidence_reference TEXT NOT NULL CHECK (evidence_reference ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{7,159}$'),
    evidence_sha256 TEXT NOT NULL CHECK (evidence_sha256 ~ '^[0-9a-f]{64}$'),
    all_attempts_ended BOOLEAN NOT NULL,
    external_reference_redacted TEXT NOT NULL DEFAULT '' CHECK (external_reference_redacted='' OR external_reference_redacted ~ '^\*{3}[A-Za-z0-9]{1,8}$'),
    proposer_user_id UUID NOT NULL,
    proposer_binding_id UUID NOT NULL,
    proposer_authorization_version BIGINT NOT NULL CHECK (proposer_authorization_version>0),
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE(case_id,case_version),
    CHECK (outcome='UNCONFIRMED' OR all_attempts_ended),
    CHECK (outcome='SUCCEEDED' OR external_reference_redacted='')
);
ALTER TABLE public.commerce_customer_repair_cases ADD CONSTRAINT commerce_repair_approved_observation_fk
    FOREIGN KEY (approved_observation_id) REFERENCES public.commerce_customer_repair_observations(id);
CREATE TRIGGER commerce_repair_observations_immutable BEFORE UPDATE OR DELETE ON public.commerce_customer_repair_observations
    FOR EACH ROW EXECUTE FUNCTION public.reject_account_manual_review_evidence_mutation();

CREATE TABLE public.commerce_customer_repair_callbacks (
    id UUID PRIMARY KEY,
    observation_sequence BIGINT GENERATED ALWAYS AS IDENTITY NOT NULL UNIQUE,
    case_id UUID NOT NULL REFERENCES public.commerce_customer_repair_cases(id),
    attempts INTEGER NOT NULL CHECK (attempts>0),
    kind TEXT NOT NULL CHECK (kind IN ('SUCCEEDED','PROVIDER_ERROR')),
    external_reference_redacted TEXT NOT NULL CHECK (external_reference_redacted='' OR external_reference_redacted ~ '^\*{3}[A-Za-z0-9]{1,8}$'),
    callback_digest TEXT NOT NULL CHECK (callback_digest ~ '^[0-9a-f]{64}$'),
    after_completion BOOLEAN NOT NULL,
    recorded_at TIMESTAMPTZ NOT NULL,
    UNIQUE(case_id,attempts,callback_digest)
);
CREATE INDEX commerce_customer_repair_callbacks_page_idx ON public.commerce_customer_repair_callbacks(case_id,observation_sequence);
CREATE TRIGGER commerce_repair_callbacks_immutable BEFORE UPDATE OR DELETE ON public.commerce_customer_repair_callbacks
    FOR EACH ROW EXECUTE FUNCTION public.reject_account_manual_review_evidence_mutation();

-- The receipt and original binding cannot be rewritten after completion.
CREATE FUNCTION public.guard_commerce_customer_repair_case() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='DELETE' OR OLD.status='RESOLVED' OR
       ROW(NEW.id,NEW.job_id,NEW.mapping_id,NEW.mapping_version,NEW.subject_type,NEW.subject_id,
           NEW.subject_lifecycle_version,NEW.provider_id,NEW.adapter_version,NEW.task_category,NEW.attempts,NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.id,OLD.job_id,OLD.mapping_id,OLD.mapping_version,OLD.subject_type,OLD.subject_id,
           OLD.subject_lifecycle_version,OLD.provider_id,OLD.adapter_version,OLD.task_category,OLD.attempts,OLD.created_at) THEN
        RAISE EXCEPTION 'commerce repair binding and completion are immutable' USING ERRCODE='55000';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER commerce_repair_case_guard BEFORE UPDATE OR DELETE ON public.commerce_customer_repair_cases
    FOR EACH ROW EXECUTE FUNCTION public.guard_commerce_customer_repair_case();
REVOKE ALL ON FUNCTION public.guard_commerce_customer_repair_case() FROM PUBLIC;

INSERT INTO public.permissions(code,scope,description) VALUES
 ('platform.commerce.repairs.read','PLATFORM','Read redacted customer synchronization repair cases and receipts.'),
 ('platform.commerce.repairs.request','PLATFORM','Quarantine customer synchronization and attest controlled original-call evidence.'),
 ('platform.commerce.repairs.approve','PLATFORM','Independently verify original-call evidence and complete a customer synchronization case.');
INSERT INTO public.roles(code,scope,description) VALUES
 ('PLATFORM_COMMERCE_OPERATOR','PLATFORM','Record original customer synchronization evidence; cannot approve its completion.'),
 ('PLATFORM_COMMERCE_REVIEWER','PLATFORM','Independently verify customer synchronization completion evidence; cannot propose it.');
INSERT INTO public.role_permissions(role_code,permission_code) VALUES
 ('PLATFORM_COMMERCE_OPERATOR','platform.commerce.repairs.read'),
 ('PLATFORM_COMMERCE_OPERATOR','platform.commerce.repairs.request'),
 ('PLATFORM_COMMERCE_REVIEWER','platform.commerce.repairs.read'),
 ('PLATFORM_COMMERCE_REVIEWER','platform.commerce.repairs.approve'),
 ('PLATFORM_SUPER_ADMIN','platform.commerce.repairs.read'),
 ('PLATFORM_SUPER_ADMIN','platform.commerce.repairs.request'),
 ('PLATFORM_SUPER_ADMIN','platform.commerce.repairs.approve'),
 ('PLATFORM_AUDITOR','platform.commerce.repairs.read');
SELECT public.bump_role_assignment_scope_version(user_id,role_code,organization_id,unit_id)
FROM public.role_assignments WHERE revoked_at IS NULL AND role_code IN ('PLATFORM_SUPER_ADMIN','PLATFORM_AUDITOR');

INSERT INTO public.approval_command_policies(command_type,policy_version,approval_mode,requester_permission,risk_class,approval_count,self_approval_forbidden,snapshot_schema_version,critical_resource_fields,expires_after,execution_window,executor_identity,created_at)
SELECT command,1,'AUDIT_ONLY',permission,'GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,
       '["target_id","request_fingerprint"]'::jsonb,interval '24 hours',interval '5 minutes','requesting-domain-service',now()
FROM (VALUES ('ACCOUNT_COMMERCE_REPAIR_QUARANTINE','platform.commerce.repairs.request'),
 ('ACCOUNT_COMMERCE_REPAIR_OBSERVE','platform.commerce.repairs.request'),
 ('ACCOUNT_COMMERCE_REPAIR_CONFIRM','platform.commerce.repairs.approve')) p(command,permission);
