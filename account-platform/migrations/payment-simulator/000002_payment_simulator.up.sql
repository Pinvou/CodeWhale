CREATE TABLE simulator_orders (
    id UUID PRIMARY KEY,
    merchant_order_no TEXT NOT NULL UNIQUE,
    channel TEXT NOT NULL CHECK (channel IN ('UNIONPAY', 'ALIPAY', 'WECHAT')),
    amount_fen BIGINT NOT NULL CHECK (amount_fen > 0),
    currency TEXT NOT NULL CHECK (currency = 'CNY'),
    status TEXT NOT NULL CHECK (status IN ('PENDING', 'SUCCESS', 'FAILED', 'CLOSED', 'REFUNDED', 'PARTIALLY_REFUNDED')),
    scenario TEXT NOT NULL CHECK (scenario IN ('SUCCESS', 'FAIL', 'CANCEL', 'TIMEOUT', 'LATE_SUCCESS', 'DROP_NOTIFY', 'DUPLICATE_NOTIFY', 'BAD_AMOUNT', 'BAD_CURRENCY', 'BAD_MERCHANT', 'BAD_SIGNATURE')),
    pay_token_hash BYTEA NOT NULL UNIQUE,
    channel_transaction_id TEXT UNIQUE,
    callback_url TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    paid_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE simulator_notifications (
    id UUID PRIMARY KEY,
    order_id UUID NOT NULL REFERENCES simulator_orders(id),
    notification_id UUID NOT NULL UNIQUE,
    payload JSONB NOT NULL,
    signature TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('PENDING', 'DELIVERED', 'ABANDONED')),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    remaining_deliveries INTEGER NOT NULL DEFAULT 1 CHECK (remaining_deliveries > 0),
    next_attempt_at TIMESTAMPTZ NOT NULL,
    delivered_at TIMESTAMPTZ,
    last_error TEXT,
    created_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX simulator_notifications_pending_idx
    ON simulator_notifications (next_attempt_at)
    WHERE status = 'PENDING';

CREATE TABLE simulator_refunds (
    id UUID PRIMARY KEY,
    order_id UUID NOT NULL REFERENCES simulator_orders(id),
    merchant_refund_no TEXT NOT NULL UNIQUE,
    amount_fen BIGINT NOT NULL CHECK (amount_fen > 0),
    status TEXT NOT NULL CHECK (status IN ('SUCCESS', 'FAILED')),
    created_at TIMESTAMPTZ NOT NULL
);
