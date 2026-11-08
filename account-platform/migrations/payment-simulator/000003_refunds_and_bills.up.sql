ALTER TABLE simulator_orders
    ADD COLUMN refunded_amount_fen BIGINT NOT NULL DEFAULT 0 CHECK (refunded_amount_fen >= 0 AND refunded_amount_fen <= amount_fen);

ALTER TABLE simulator_orders DROP CONSTRAINT simulator_orders_scenario_check;
ALTER TABLE simulator_orders ADD CONSTRAINT simulator_orders_scenario_check
    CHECK (scenario IN ('SUCCESS', 'FAIL', 'CANCEL', 'TIMEOUT', 'LATE_SUCCESS', 'DROP_NOTIFY', 'DUPLICATE_NOTIFY', 'BAD_AMOUNT', 'BAD_CURRENCY', 'BAD_MERCHANT', 'BAD_SIGNATURE', 'REFUND_FAIL'));

ALTER TABLE simulator_refunds
    ADD COLUMN channel_refund_id TEXT UNIQUE,
    ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT now();

CREATE INDEX simulator_orders_paid_at_idx
    ON simulator_orders (channel, paid_at)
    WHERE status IN ('SUCCESS', 'PARTIALLY_REFUNDED', 'REFUNDED');
