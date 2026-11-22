CREATE TABLE entitlement_packages (
    id UUID PRIMARY KEY,
    user_id UUID NOT NULL,
    wallet_id UUID NOT NULL REFERENCES wallets(id),
    payment_order_id UUID NOT NULL UNIQUE REFERENCES payment_orders(id),
    credit_lot_id UUID NOT NULL UNIQUE REFERENCES credit_lots(id),
    product_id TEXT NOT NULL,
    plan_name TEXT NOT NULL,
    sku_code TEXT NOT NULL,
    amount_fen BIGINT NOT NULL CHECK (amount_fen > 0),
    points_total BIGINT NOT NULL CHECK (points_total > 0),
    points_reserved BIGINT NOT NULL DEFAULT 0 CHECK (points_reserved >= 0),
    points_used BIGINT NOT NULL DEFAULT 0 CHECK (points_used >= 0),
    points_remaining BIGINT NOT NULL CHECK (points_remaining >= 0),
    credited_at TIMESTAMPTZ NOT NULL,
    valid_from TIMESTAMPTZ NOT NULL,
    valid_until TIMESTAMPTZ NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('ACTIVE', 'CONSUMED', 'EXPIRED', 'CREDIT_FAILED', 'REPAIRED', 'FROZEN')),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CHECK (valid_until > valid_from),
    CHECK (points_reserved + points_used + points_remaining = points_total)
);

CREATE INDEX entitlement_packages_wallet_active_idx
    ON entitlement_packages (wallet_id, valid_until, credited_at, id)
    WHERE status = 'ACTIVE';

CREATE INDEX entitlement_packages_user_history_idx
    ON entitlement_packages (user_id, credited_at DESC, id DESC);
