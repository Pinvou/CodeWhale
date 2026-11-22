DROP INDEX payment_orders_reconciliation_idx;

ALTER TABLE payment_orders
    DROP COLUMN reconciliation_last_error,
    DROP COLUMN reconciliation_lease_until,
    DROP COLUMN reconciliation_next_at,
    DROP COLUMN reconciliation_attempts;
