ALTER TABLE usage_replay_results
    ADD COLUMN evidence_payload_digest TEXT
        CHECK (evidence_payload_digest IS NULL OR evidence_payload_digest ~ '^[0-9a-f]{64}$');

CREATE TABLE usage_convergence_evidence (
    id UUID PRIMARY KEY,
    usage_request_id UUID NOT NULL REFERENCES usage_requests(id) ON DELETE CASCADE,
    evidence_type TEXT NOT NULL CHECK (evidence_type IN (
        'DISPATCH_STARTED','UPSTREAM_REFERENCE','RESULT_OBSERVED','PROVIDER_LOOKUP',
        'UNCERTAINTY','CANCELLATION','SETTLEMENT','CUSTOMER_HOLD_RELEASE','RECONCILIATION_STOPPED'
    )),
    source_type TEXT NOT NULL CHECK (source_type IN ('GATEWAY','PROVIDER_QUERY','BILLING','RECONCILER')),
    decision TEXT NOT NULL CHECK (decision IN ('RECORDED','REVIEW','COMMIT','RELEASE')),
    evidence_key TEXT NOT NULL,
    provider_id TEXT,
    adapter_version TEXT,
    upstream_reference_digest TEXT CHECK (upstream_reference_digest IS NULL OR upstream_reference_digest ~ '^[0-9a-f]{64}$'),
    payload_digest TEXT NOT NULL CHECK (payload_digest ~ '^[0-9a-f]{64}$'),
    sanitized_snapshot JSONB NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    UNIQUE(usage_request_id,evidence_type,evidence_key,payload_digest)
);

CREATE INDEX usage_convergence_evidence_timeline_idx
    ON usage_convergence_evidence(usage_request_id,observed_at,id);

CREATE TRIGGER usage_convergence_evidence_immutable
BEFORE UPDATE OR DELETE ON usage_convergence_evidence
FOR EACH ROW EXECUTE FUNCTION reject_usage_command_or_evidence_mutation();
