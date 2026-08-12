CREATE TABLE payment_transactions (
    id UUID PRIMARY KEY,
    payment_order_id UUID NOT NULL REFERENCES payment_orders(id),
    channel TEXT NOT NULL CHECK (channel IN ('UNIONPAY', 'ALIPAY', 'WECHAT')),
    channel_transaction_id TEXT NOT NULL UNIQUE,
    status TEXT NOT NULL CHECK (status IN ('SUCCEEDED', 'REFUNDED', 'PARTIALLY_REFUNDED', 'MANUAL_REVIEW')),
    amount_fen BIGINT NOT NULL CHECK (amount_fen > 0),
    currency TEXT NOT NULL CHECK (currency = 'CNY'),
    source TEXT NOT NULL CHECK (source IN ('NOTIFICATION', 'QUERY', 'RECONCILIATION')),
    verified_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE UNIQUE INDEX payment_transactions_order_idx
    ON payment_transactions (payment_order_id);

CREATE TABLE refunds (
    id UUID PRIMARY KEY,
    payment_order_id UUID NOT NULL REFERENCES payment_orders(id),
    requester_id UUID NOT NULL,
    merchant_refund_no TEXT NOT NULL UNIQUE,
    channel_refund_id TEXT UNIQUE,
    amount_fen BIGINT NOT NULL CHECK (amount_fen > 0),
    credit_microcredits BIGINT NOT NULL CHECK (credit_microcredits > 0),
    status TEXT NOT NULL CHECK (status IN ('REQUESTED', 'SUCCEEDED', 'FAILED', 'MANUAL_REVIEW')),
    idempotency_key TEXT NOT NULL UNIQUE,
    request_hash TEXT NOT NULL,
    reason TEXT NOT NULL,
    failure_reason TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX refunds_order_idx ON refunds (payment_order_id, created_at);

CREATE TABLE manual_adjustments (
    id UUID PRIMARY KEY,
    wallet_id UUID NOT NULL REFERENCES wallets(id),
    amount_microcredits BIGINT NOT NULL CHECK (amount_microcredits <> 0),
    reason TEXT NOT NULL,
    requested_by UUID NOT NULL,
    approved_by UUID,
    status TEXT NOT NULL CHECK (status IN ('PENDING_APPROVAL', 'APPROVED', 'REJECTED', 'POSTED')),
    journal_entry_id UUID REFERENCES journal_entries(id),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CHECK (approved_by IS NULL OR approved_by <> requested_by),
    CHECK (status NOT IN ('APPROVED', 'POSTED') OR approved_by IS NOT NULL)
);

CREATE TABLE reconciliation_runs (
    id UUID PRIMARY KEY,
    channel TEXT NOT NULL CHECK (channel IN ('UNIONPAY', 'ALIPAY', 'WECHAT')),
    business_date DATE NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('RUNNING', 'COMPLETED', 'FAILED')),
    provider_row_count INTEGER NOT NULL DEFAULT 0 CHECK (provider_row_count >= 0),
    case_count INTEGER NOT NULL DEFAULT 0 CHECK (case_count >= 0),
    started_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,
    error_message TEXT,
    UNIQUE (channel, business_date)
);

CREATE TABLE reconciliation_cases (
    id UUID PRIMARY KEY,
    reconciliation_run_id UUID NOT NULL REFERENCES reconciliation_runs(id),
    payment_order_id UUID REFERENCES payment_orders(id),
    merchant_order_no TEXT NOT NULL,
    case_type TEXT NOT NULL CHECK (case_type IN ('PROVIDER_PAID_INTERNAL_MISSING', 'INTERNAL_PAID_PROVIDER_MISSING', 'AMOUNT_MISMATCH', 'DUPLICATE_TRANSACTION')),
    status TEXT NOT NULL CHECK (status IN ('OPEN', 'RESOLVED', 'IGNORED')),
    provider_snapshot JSONB,
    internal_snapshot JSONB,
    resolution TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    resolved_at TIMESTAMPTZ
);

CREATE INDEX reconciliation_cases_open_idx
    ON reconciliation_cases (case_type, created_at)
    WHERE status = 'OPEN';
