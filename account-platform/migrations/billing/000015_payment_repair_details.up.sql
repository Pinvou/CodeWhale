ALTER TABLE payment_order_repairs
    ADD COLUMN entitlement_package_id UUID NOT NULL UNIQUE REFERENCES entitlement_packages(id),
    ADD COLUMN points_awarded BIGINT NOT NULL CHECK (points_awarded > 0),
    ADD COLUMN wallet_balance_before BIGINT NOT NULL CHECK (wallet_balance_before >= 0),
    ADD COLUMN wallet_balance_after BIGINT NOT NULL CHECK (wallet_balance_after >= wallet_balance_before);
