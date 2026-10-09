ALTER TABLE usage_requests
    ADD COLUMN cancellation_origin TEXT NOT NULL DEFAULT 'NONE'
        CHECK (cancellation_origin IN ('NONE','USER','MODEL_EMERGENCY'));

CREATE TABLE model_emergency_cancellation_policies (
    id UUID PRIMARY KEY,
    catalog_version BIGINT NOT NULL REFERENCES model_catalog_versions(version),
    model_id UUID NOT NULL REFERENCES model_identities(id),
    effective_at TIMESTAMPTZ NOT NULL,
    requested_by UUID NOT NULL,
    reason TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE(catalog_version,model_id)
);

CREATE TABLE model_emergency_cancellations (
    id UUID PRIMARY KEY,
    policy_id UUID NOT NULL REFERENCES model_emergency_cancellation_policies(id),
    usage_request_id UUID NOT NULL UNIQUE REFERENCES usage_requests(id),
    status TEXT NOT NULL CHECK (status IN ('PENDING','LEASED','CONVERGING','CONVERGED')),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts>=0),
    lease_until TIMESTAMPTZ,
    cancellation_capability TEXT,
    local_transport_aborted BOOLEAN,
    provider_cancel_result TEXT,
    hold_id UUID,
    actual_microcredits BIGINT CHECK (actual_microcredits IS NULL OR actual_microcredits>=0),
    final_execution_status TEXT,
    final_cancel_status TEXT,
    final_settlement_status TEXT,
    final_usage_snapshot JSONB,
    requested_at TIMESTAMPTZ NOT NULL,
    attempted_at TIMESTAMPTZ,
    converged_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX model_emergency_cancellations_work_idx
    ON model_emergency_cancellations(status,lease_until,updated_at,id)
    WHERE status<>'CONVERGED';

CREATE TABLE model_emergency_cancellation_evidence (
    id UUID PRIMARY KEY,
    emergency_cancellation_id UUID NOT NULL REFERENCES model_emergency_cancellations(id),
    evidence_type TEXT NOT NULL CHECK (evidence_type IN ('REQUESTED','ATTEMPTED','CONVERGED')),
    snapshot JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX model_emergency_cancellation_evidence_timeline_idx
    ON model_emergency_cancellation_evidence(emergency_cancellation_id,created_at,id);

CREATE TRIGGER model_emergency_cancellation_policies_immutable
BEFORE UPDATE OR DELETE ON model_emergency_cancellation_policies
FOR EACH ROW EXECUTE FUNCTION reject_usage_command_or_evidence_mutation();
CREATE TRIGGER model_emergency_cancellation_evidence_immutable
BEFORE UPDATE OR DELETE ON model_emergency_cancellation_evidence
FOR EACH ROW EXECUTE FUNCTION reject_usage_command_or_evidence_mutation();
