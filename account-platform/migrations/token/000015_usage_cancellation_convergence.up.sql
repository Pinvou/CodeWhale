ALTER TABLE usage_requests DROP CONSTRAINT usage_requests_execution_status_check;
ALTER TABLE usage_requests
    ADD CONSTRAINT usage_requests_execution_status_check CHECK (execution_status IN (
        'RECEIVED','HOLD_RESERVING','HOLD_RESERVED','DISPATCH_INTENT_PERSISTED',
        'UPSTREAM_DISPATCHING','STREAMING','SETTLEMENT_PENDING','SUCCEEDED','FAILED',
        'CANCELLED_BEFORE_DISPATCH','REVIEW'
    )),
    ADD COLUMN cancel_status TEXT NOT NULL DEFAULT 'NOT_REQUESTED'
        CHECK (cancel_status IN ('NOT_REQUESTED','CANCEL_REQUESTED','LOCAL_CONFIRMED','PROVIDER_SENT','PROVIDER_CONFIRMED','PROVIDER_REJECTED','UNSUPPORTED','RESULT_UNKNOWN','ALREADY_TERMINAL')),
    ADD COLUMN settlement_status TEXT NOT NULL DEFAULT 'HOLD_PENDING'
        CHECK (settlement_status IN ('HOLD_PENDING','HOLD_ACTIVE','USAGE_PENDING','SETTLED','RELEASED','REVIEW')),
    ADD COLUMN cancel_requested_at TIMESTAMPTZ,
    ADD COLUMN provider_cancel_sent_at TIMESTAMPTZ,
    ADD COLUMN provider_cancel_confirmed_at TIMESTAMPTZ,
    ADD COLUMN provider_terminal_at TIMESTAMPTZ,
    ADD COLUMN final_usage_observed_at TIMESTAMPTZ,
    ADD COLUMN user_stopped_at TIMESTAMPTZ,
    ADD COLUMN last_persisted_sequence BIGINT NOT NULL DEFAULT 0 CHECK (last_persisted_sequence >= 0),
    ADD COLUMN last_client_confirmed_sequence BIGINT NOT NULL DEFAULT 0 CHECK (last_client_confirmed_sequence >= 0),
    ADD COLUMN trusted_usage_snapshot JSONB,
    ADD COLUMN trusted_usage_source TEXT,
    ADD COLUMN cancel_reconciliation_deadline TIMESTAMPTZ,
    ADD COLUMN convergence_policy_version INTEGER NOT NULL DEFAULT 1 CHECK (convergence_policy_version > 0),
    ADD COLUMN maximum_execution_seconds INTEGER NOT NULL DEFAULT 7200 CHECK (maximum_execution_seconds BETWEEN 60 AND 86400),
    ADD COLUMN final_usage_delay_seconds INTEGER NOT NULL DEFAULT 79200 CHECK (final_usage_delay_seconds BETWEEN 0 AND 604800),
    ADD COLUMN cancel_reconciliation_seconds INTEGER NOT NULL DEFAULT 86400 CHECK (cancel_reconciliation_seconds >= maximum_execution_seconds + final_usage_delay_seconds);

UPDATE usage_requests
SET settlement_status=CASE status
    WHEN 'COMMITTED' THEN 'SETTLED'
    WHEN 'RELEASED' THEN 'RELEASED'
    WHEN 'SETTLEMENT_PENDING' THEN 'USAGE_PENDING'
    WHEN 'REVIEW' THEN 'REVIEW'
    WHEN 'RESERVED' THEN 'HOLD_ACTIVE'
    WHEN 'UPSTREAM_ACTIVE' THEN 'USAGE_PENDING'
    ELSE 'HOLD_PENDING'
END,
provider_terminal_at=CASE WHEN execution_status IN ('SETTLEMENT_PENDING','SUCCEEDED','FAILED','REVIEW') THEN updated_at END,
final_usage_observed_at=CASE WHEN actual_microcredits IS NOT NULL THEN updated_at END,
trusted_usage_snapshot=CASE WHEN usage_snapshot IS NOT NULL AND usage_snapshot <> '{}'::jsonb THEN usage_snapshot END,
trusted_usage_source=CASE WHEN actual_microcredits IS NOT NULL THEN 'MIGRATED_SETTLEMENT' END;

CREATE TABLE usage_cancel_commands (
    id UUID PRIMARY KEY,
    usage_request_id UUID NOT NULL UNIQUE REFERENCES usage_requests(id) ON DELETE CASCADE,
    user_id UUID NOT NULL,
    idempotency_key_digest TEXT NOT NULL CHECK (length(idempotency_key_digest)=64),
    request_fingerprint TEXT NOT NULL CHECK (length(request_fingerprint)=64),
    cancellation_version INTEGER NOT NULL DEFAULT 1 CHECK (cancellation_version > 0),
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE(user_id,idempotency_key_digest)
);

CREATE TABLE usage_cancellation_evidence (
    id UUID PRIMARY KEY,
    usage_request_id UUID NOT NULL REFERENCES usage_requests(id) ON DELETE CASCADE,
    evidence_type TEXT NOT NULL,
    provider_id TEXT,
    adapter_version TEXT NOT NULL,
    upstream_reference_digest TEXT,
    sanitized_evidence JSONB NOT NULL DEFAULT '{}'::jsonb,
    occurred_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE usage_delivery_confirmations (
    usage_request_id UUID NOT NULL REFERENCES usage_requests(id) ON DELETE CASCADE,
    delivery_id_digest TEXT NOT NULL CHECK (length(delivery_id_digest)=64),
    last_confirmed_sequence BIGINT NOT NULL CHECK (last_confirmed_sequence >= 0),
    confirmed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY(usage_request_id,delivery_id_digest)
);

CREATE TABLE usage_reconciliation_commands (
    id UUID PRIMARY KEY,
    usage_request_id UUID NOT NULL REFERENCES usage_requests(id) ON DELETE CASCADE,
    actor_user_id UUID NOT NULL,
    idempotency_key_digest TEXT NOT NULL CHECK (length(idempotency_key_digest)=64),
    request_fingerprint TEXT NOT NULL CHECK (length(request_fingerprint)=64),
    reason TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE(actor_user_id,idempotency_key_digest)
);

CREATE TABLE usage_platform_loss_reviews (
    id UUID PRIMARY KEY,
    usage_request_id UUID NOT NULL UNIQUE REFERENCES usage_requests(id),
    cause_code TEXT NOT NULL,
    provider_cost_evidence JSONB NOT NULL DEFAULT '{}'::jsonb,
    customer_debit_prohibited BOOLEAN NOT NULL DEFAULT true CHECK (customer_debit_prohibited),
    created_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX usage_cancel_reconciliation_due_idx
    ON usage_requests(cancel_reconciliation_deadline,updated_at)
    WHERE cancel_status IN ('CANCEL_REQUESTED','PROVIDER_SENT','PROVIDER_CONFIRMED','RESULT_UNKNOWN');

CREATE OR REPLACE FUNCTION reject_usage_command_or_evidence_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'usage command and evidence records are immutable';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER usage_cancel_commands_immutable BEFORE UPDATE OR DELETE ON usage_cancel_commands
FOR EACH ROW EXECUTE FUNCTION reject_usage_command_or_evidence_mutation();
CREATE TRIGGER usage_cancellation_evidence_immutable BEFORE UPDATE OR DELETE ON usage_cancellation_evidence
FOR EACH ROW EXECUTE FUNCTION reject_usage_command_or_evidence_mutation();
CREATE TRIGGER usage_reconciliation_commands_immutable BEFORE UPDATE OR DELETE ON usage_reconciliation_commands
FOR EACH ROW EXECUTE FUNCTION reject_usage_command_or_evidence_mutation();
CREATE TRIGGER usage_platform_loss_reviews_immutable BEFORE UPDATE OR DELETE ON usage_platform_loss_reviews
FOR EACH ROW EXECUTE FUNCTION reject_usage_command_or_evidence_mutation();
