CREATE TABLE payment_product_resume_executions (
    id UUID PRIMARY KEY,
    approval_request_id UUID NOT NULL UNIQUE,
    provider_id TEXT NOT NULL,
    provider_version TEXT NOT NULL,
    product_id TEXT NOT NULL,
    probe_order_id UUID NOT NULL REFERENCES payment_orders(id),
    requester_user_id UUID NOT NULL,
    approver_user_id UUID NOT NULL,
    decision_reason TEXT NOT NULL,
    provider_status TEXT NOT NULL,
    provider_trace_id TEXT NOT NULL,
    control_versions_before JSONB NOT NULL,
    executed_at TIMESTAMPTZ NOT NULL,
    CHECK (requester_user_id<>approver_user_id),
    CHECK (length(decision_reason)>=10)
);

CREATE OR REPLACE FUNCTION reject_payment_product_resume_execution_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'payment product resume executions are immutable';
END $$;
CREATE TRIGGER payment_product_resume_executions_immutable BEFORE UPDATE OR DELETE ON payment_product_resume_executions FOR EACH ROW EXECUTE FUNCTION reject_payment_product_resume_execution_mutation();
