CREATE TABLE payment_anomaly_resolution_intents (
    payment_anomaly_case_id UUID PRIMARY KEY REFERENCES payment_anomaly_cases(id),
    actor_id UUID NOT NULL,
    idempotency_key TEXT NOT NULL,
    request_hash TEXT NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    resolution TEXT NOT NULL,
    reason TEXT NOT NULL,
    target_snapshot JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);
CREATE TRIGGER payment_anomaly_resolution_intents_immutable BEFORE UPDATE OR DELETE ON payment_anomaly_resolution_intents
FOR EACH ROW EXECUTE FUNCTION reject_payment_anomaly_command_mutation();

