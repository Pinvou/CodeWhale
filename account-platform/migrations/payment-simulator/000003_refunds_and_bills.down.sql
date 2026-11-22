DROP INDEX IF EXISTS simulator_orders_paid_at_idx;
ALTER TABLE simulator_refunds DROP COLUMN IF EXISTS updated_at;
ALTER TABLE simulator_refunds DROP COLUMN IF EXISTS channel_refund_id;
ALTER TABLE simulator_orders DROP CONSTRAINT simulator_orders_scenario_check;
ALTER TABLE simulator_orders ADD CONSTRAINT simulator_orders_scenario_check
    CHECK (scenario IN ('SUCCESS', 'FAIL', 'CANCEL', 'TIMEOUT', 'LATE_SUCCESS', 'DROP_NOTIFY', 'DUPLICATE_NOTIFY', 'BAD_AMOUNT', 'BAD_CURRENCY', 'BAD_MERCHANT', 'BAD_SIGNATURE'));
ALTER TABLE simulator_orders DROP COLUMN IF EXISTS refunded_amount_fen;
