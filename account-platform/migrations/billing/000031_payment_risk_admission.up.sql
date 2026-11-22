CREATE TABLE payment_risk_attempts (
    id UUID PRIMARY KEY,
    action TEXT NOT NULL CHECK (action IN ('CREATE','ORDER_QUERY','REFUND','CALLBACK')),
    payer_user_id UUID,
    beneficiary_organization_id UUID,
    source_hash BYTEA NOT NULL CHECK (octet_length(source_hash)=32),
    order_id UUID REFERENCES payment_orders(id),
    payment_transaction_id UUID REFERENCES payment_transactions(id),
    entitlement_package_id UUID REFERENCES entitlement_packages(id),
    provider_id TEXT NOT NULL,
    result TEXT NOT NULL CHECK (result IN ('ACCEPTED','RATE_LIMITED','OVER_CAPACITY')),
    provider_dispatched BOOLEAN NOT NULL DEFAULT FALSE,
    occurred_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX payment_risk_payer_idx ON payment_risk_attempts(action,payer_user_id,occurred_at DESC);
CREATE INDEX payment_risk_organization_idx ON payment_risk_attempts(action,beneficiary_organization_id,occurred_at DESC);
CREATE INDEX payment_risk_source_idx ON payment_risk_attempts(action,source_hash,occurred_at DESC);
CREATE INDEX payment_risk_order_idx ON payment_risk_attempts(action,order_id,occurred_at DESC);
CREATE INDEX payment_risk_provider_idx ON payment_risk_attempts(action,provider_id,occurred_at DESC);
