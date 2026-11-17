-- Governance counts and complete fact fingerprints share this active set.
CREATE INDEX usage_governance_account_credential_idx
    ON public.usage_requests(token_account_id,credential_id)
    WHERE execution_status IN ('RECEIVED','HOLD_RESERVING','HOLD_RESERVED',
        'DISPATCH_INTENT_PERSISTED','UPSTREAM_DISPATCHING','STREAMING','SETTLEMENT_PENDING');
CREATE INDEX usage_governance_model_idx
    ON public.usage_requests(model_id)
    WHERE execution_status IN ('RECEIVED','HOLD_RESERVING','HOLD_RESERVED',
        'DISPATCH_INTENT_PERSISTED','UPSTREAM_DISPATCHING','STREAMING','SETTLEMENT_PENDING');
