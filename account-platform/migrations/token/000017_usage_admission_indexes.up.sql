CREATE INDEX usage_requests_user_admission_idx ON usage_requests(user_id,started_at DESC);
CREATE INDEX usage_requests_organization_admission_idx ON usage_requests(organization_id,started_at DESC) WHERE organization_id IS NOT NULL;
CREATE INDEX usage_requests_model_admission_idx ON usage_requests(model,started_at DESC);
CREATE INDEX usage_requests_route_admission_idx ON usage_requests(token_account_id,started_at DESC);
CREATE INDEX usage_requests_inflight_admission_idx ON usage_requests(execution_status,started_at DESC)
    WHERE execution_status IN ('RECEIVED','HOLD_RESERVING','HOLD_RESERVED','DISPATCH_INTENT_PERSISTED','UPSTREAM_DISPATCHING','STREAMING','SETTLEMENT_PENDING');
