ALTER TABLE payment_order_repairs
    DROP COLUMN IF EXISTS wallet_balance_after,
    DROP COLUMN IF EXISTS wallet_balance_before,
    DROP COLUMN IF EXISTS points_awarded,
    DROP COLUMN IF EXISTS entitlement_package_id;
