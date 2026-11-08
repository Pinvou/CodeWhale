CREATE TABLE payment_provider_resume_executions (
    id UUID PRIMARY KEY,
    approval_request_id UUID NOT NULL UNIQUE,
    provider_id TEXT NOT NULL,
    provider_version TEXT NOT NULL,
    probe_order_id UUID NOT NULL REFERENCES payment_orders(id),
    requester_user_id UUID NOT NULL,
    approver_user_id UUID NOT NULL CHECK(requester_user_id<>approver_user_id),
    decision_reason TEXT NOT NULL CHECK(length(decision_reason)>=10),
    provider_status TEXT NOT NULL,
    provider_trace_id TEXT NOT NULL DEFAULT '',
    resumed_product_ids JSONB NOT NULL,
    control_versions_before JSONB NOT NULL,
    executed_at TIMESTAMPTZ NOT NULL
);

CREATE OR REPLACE FUNCTION protect_payment_provider_resume_execution() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'payment provider resume executions are immutable';
END $$;
CREATE TRIGGER payment_provider_resume_executions_immutable BEFORE UPDATE OR DELETE ON payment_provider_resume_executions FOR EACH ROW EXECUTE FUNCTION protect_payment_provider_resume_execution();
