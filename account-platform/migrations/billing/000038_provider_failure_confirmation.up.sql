CREATE TABLE payment_provider_failure_confirmations (
    id UUID PRIMARY KEY,
    payment_anomaly_case_id UUID NOT NULL UNIQUE REFERENCES payment_anomaly_cases(id),
    payment_order_id UUID NOT NULL REFERENCES payment_orders(id),
    provider_id TEXT NOT NULL,
    provider_version TEXT NOT NULL,
    provider_status TEXT NOT NULL CHECK (provider_status='FAILED'),
    provider_payload_digest TEXT NOT NULL CHECK (provider_payload_digest ~ '^[0-9a-f]{64}$'),
    provider_snapshot JSONB NOT NULL,
    queried_at TIMESTAMPTZ NOT NULL,
    actor_id UUID NOT NULL,
    reason TEXT NOT NULL CHECK (length(reason)>=10),
    created_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX payment_provider_failure_confirmations_order_idx
    ON payment_provider_failure_confirmations(payment_order_id,created_at,id);

CREATE OR REPLACE FUNCTION reject_payment_provider_failure_confirmation_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'payment provider failure confirmations are immutable';
END $$;

CREATE TRIGGER payment_provider_failure_confirmations_immutable
BEFORE UPDATE OR DELETE ON payment_provider_failure_confirmations
FOR EACH ROW EXECUTE FUNCTION reject_payment_provider_failure_confirmation_mutation();
