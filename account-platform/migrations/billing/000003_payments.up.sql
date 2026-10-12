CREATE TABLE payment_orders (
    id UUID PRIMARY KEY,
    merchant_order_no TEXT NOT NULL UNIQUE,
    requester_id UUID NOT NULL,
    wallet_id UUID NOT NULL REFERENCES wallets(id),
    product_id TEXT NOT NULL,
    product_snapshot JSONB NOT NULL,
    channel TEXT NOT NULL CHECK (channel IN ('UNIONPAY', 'ALIPAY', 'WECHAT')),
    amount_fen BIGINT NOT NULL CHECK (amount_fen > 0),
    currency TEXT NOT NULL CHECK (currency = 'CNY'),
    credit_microcredits BIGINT NOT NULL CHECK (credit_microcredits > 0),
    status TEXT NOT NULL CHECK (status IN ('CREATED', 'PENDING', 'PAID', 'PAID_LATE', 'FAILED', 'CLOSED', 'REFUNDED', 'PARTIALLY_REFUNDED', 'MANUAL_REVIEW')),
    fulfillment_status TEXT NOT NULL CHECK (fulfillment_status IN ('PENDING', 'CREDITED', 'REFUNDED', 'MANUAL_REVIEW')),
    payment_url TEXT,
    channel_transaction_id TEXT UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,
    paid_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX payment_orders_requester_idx ON payment_orders (requester_id, created_at DESC);
CREATE INDEX payment_orders_pending_idx ON payment_orders (expires_at) WHERE status IN ('CREATED', 'PENDING');

CREATE TABLE payment_notifications (
    notification_id UUID PRIMARY KEY,
    channel TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    payload JSONB NOT NULL,
    received_at TIMESTAMPTZ NOT NULL,
    processed_at TIMESTAMPTZ,
    result TEXT NOT NULL CHECK (result IN ('RECEIVED', 'PROCESSED', 'REJECTED'))
);
