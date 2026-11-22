ALTER TABLE usage_requests
    RENAME COLUMN idempotency_key TO legacy_idempotency_key;

ALTER TABLE usage_requests
    ADD COLUMN idempotency_namespace TEXT NOT NULL DEFAULT 'model-inference'
        CHECK (idempotency_namespace = 'model-inference'),
    ADD COLUMN idempotency_key_digest TEXT,
    ADD COLUMN canonicalization_version INTEGER NOT NULL DEFAULT 0 CHECK (canonicalization_version >= 0),
    ADD COLUMN request_fingerprint TEXT,
    ADD COLUMN execution_status TEXT NOT NULL DEFAULT 'RECEIVED'
        CHECK (execution_status IN ('RECEIVED','HOLD_RESERVING','HOLD_RESERVED','DISPATCH_INTENT_PERSISTED','UPSTREAM_DISPATCHING','STREAMING','SETTLEMENT_PENDING','SUCCEEDED','FAILED','REVIEW')),
    ADD COLUMN execution_version BIGINT NOT NULL DEFAULT 1 CHECK (execution_version > 0),
    ADD COLUMN delivery_status TEXT NOT NULL DEFAULT 'NOT_STARTED'
        CHECK (delivery_status IN ('NOT_STARTED','DELIVERING','DISCONNECTED','DELIVERED','USER_STOPPED')),
    ADD COLUMN dispatch_lease_owner TEXT,
    ADD COLUMN dispatch_lease_until TIMESTAMPTZ;

UPDATE usage_requests
SET idempotency_key_digest=encode(digest(convert_to(legacy_idempotency_key,'UTF8'),'sha256'),'hex'),
    request_fingerprint=COALESCE(request_snapshot->>'request_sha256',encode(digest(convert_to(request_snapshot::text,'UTF8'),'sha256'),'hex')),
    execution_status=CASE status
        WHEN 'CREATED' THEN 'RECEIVED'
        WHEN 'RESERVED' THEN 'HOLD_RESERVED'
        WHEN 'UPSTREAM_ACTIVE' THEN 'UPSTREAM_DISPATCHING'
        WHEN 'SETTLEMENT_PENDING' THEN 'SETTLEMENT_PENDING'
        WHEN 'COMMITTED' THEN 'SUCCEEDED'
        WHEN 'RELEASED' THEN 'FAILED'
        ELSE 'REVIEW'
    END;

ALTER TABLE usage_requests
    ALTER COLUMN request_fingerprint SET NOT NULL;

ALTER TABLE usage_requests DROP CONSTRAINT usage_requests_idempotency_key_key;
ALTER TABLE usage_requests DROP COLUMN legacy_idempotency_key;

CREATE UNIQUE INDEX usage_requests_inference_idempotency_idx
    ON usage_requests(user_id,idempotency_namespace,idempotency_key_digest)
    WHERE idempotency_key_digest IS NOT NULL;

CREATE TABLE usage_execution_transitions (
    id UUID PRIMARY KEY,
    usage_request_id UUID NOT NULL REFERENCES usage_requests(id) ON DELETE CASCADE,
    from_status TEXT NOT NULL,
    to_status TEXT NOT NULL,
    from_version BIGINT NOT NULL,
    to_version BIGINT NOT NULL,
    reason TEXT NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    UNIQUE(usage_request_id,to_version)
);

CREATE TABLE usage_dispatch_intents (
    usage_request_id UUID PRIMARY KEY REFERENCES usage_requests(id) ON DELETE CASCADE,
    encrypted_envelope BYTEA,
    key_id TEXT,
    envelope_integrity TEXT,
    upstream_idempotency_key TEXT NOT NULL,
    adapter_supports_idempotency BOOLEAN NOT NULL DEFAULT false,
    worker_lease_owner TEXT,
    worker_lease_until TIMESTAMPTZ,
    worker_acquired_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    CHECK (encrypted_envelope IS NULL OR length(encrypted_envelope) > 0),
    CHECK (length(upstream_idempotency_key) >= 16)
);

CREATE TABLE usage_dispatch_preparations (
    usage_request_id UUID PRIMARY KEY REFERENCES usage_requests(id) ON DELETE CASCADE,
    encrypted_envelope BYTEA NOT NULL,
    key_id TEXT NOT NULL,
    envelope_integrity TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CHECK (length(encrypted_envelope) > 0)
);

CREATE TABLE usage_stream_events (
    usage_request_id UUID NOT NULL REFERENCES usage_requests(id) ON DELETE CASCADE,
    sequence BIGINT NOT NULL CHECK (sequence > 0),
    event_type TEXT NOT NULL,
    encrypted_payload BYTEA NOT NULL,
    key_id TEXT NOT NULL,
    payload_integrity TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ,
    PRIMARY KEY(usage_request_id,sequence)
);

CREATE INDEX usage_stream_events_expiry_idx ON usage_stream_events(expires_at);

CREATE TABLE usage_replay_results (
    usage_request_id UUID PRIMARY KEY REFERENCES usage_requests(id) ON DELETE CASCADE,
    response_kind TEXT NOT NULL CHECK (response_kind IN ('JSON','STREAM_AGGREGATE')),
    http_status INTEGER NOT NULL CHECK (http_status BETWEEN 100 AND 599),
    content_type TEXT NOT NULL,
    encrypted_payload BYTEA NOT NULL,
    key_id TEXT NOT NULL,
    payload_integrity TEXT NOT NULL,
    final_usage JSONB,
    last_sequence BIGINT NOT NULL DEFAULT 0 CHECK (last_sequence >= 0),
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ
);

CREATE INDEX usage_replay_results_expiry_idx ON usage_replay_results(expires_at);

CREATE TABLE usage_idempotency_tombstones (
    usage_request_id UUID PRIMARY KEY REFERENCES usage_requests(id) ON DELETE CASCADE,
    user_id UUID NOT NULL,
    idempotency_namespace TEXT NOT NULL,
    idempotency_key_digest TEXT NOT NULL,
    canonicalization_version INTEGER NOT NULL,
    request_fingerprint TEXT NOT NULL,
    terminal_status TEXT NOT NULL,
    result_summary JSONB NOT NULL DEFAULT '{}'::jsonb,
    charge_references JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at <= created_at + interval '90 days'),
    UNIQUE(user_id,idempotency_namespace,idempotency_key_digest)
);

CREATE INDEX usage_idempotency_tombstones_expiry_idx ON usage_idempotency_tombstones(expires_at);

CREATE TABLE usage_replay_retention_cases (
    id UUID PRIMARY KEY,
    usage_request_id UUID NOT NULL REFERENCES usage_requests(id),
    reason TEXT NOT NULL,
    scope TEXT NOT NULL,
    owner_id UUID NOT NULL,
    review_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    released_at TIMESTAMPTZ,
    clear_by TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    CHECK (released_at IS NULL OR clear_by IS NOT NULL AND clear_by <= released_at + interval '30 days')
);

CREATE INDEX usage_replay_retention_cases_active_idx
    ON usage_replay_retention_cases(usage_request_id,expires_at)
    WHERE released_at IS NULL;

CREATE TABLE usage_replay_case_payloads (
    retention_case_id UUID PRIMARY KEY REFERENCES usage_replay_retention_cases(id) ON DELETE CASCADE,
    encrypted_events JSONB NOT NULL DEFAULT '[]'::jsonb,
    encrypted_result JSONB,
    moved_at TIMESTAMPTZ NOT NULL,
    CHECK (jsonb_typeof(encrypted_events) = 'array')
);

CREATE OR REPLACE FUNCTION reject_usage_execution_history_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'usage execution history is immutable';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER usage_execution_transitions_immutable
BEFORE UPDATE OR DELETE ON usage_execution_transitions
FOR EACH ROW EXECUTE FUNCTION reject_usage_execution_history_mutation();
