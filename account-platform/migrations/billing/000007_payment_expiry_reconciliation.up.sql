ALTER TABLE payment_orders
    ADD COLUMN reconciliation_attempts INTEGER NOT NULL DEFAULT 0 CHECK (reconciliation_attempts >= 0),
    ADD COLUMN reconciliation_next_at TIMESTAMPTZ,
    ADD COLUMN reconciliation_lease_until TIMESTAMPTZ,
    ADD COLUMN reconciliation_last_error TEXT;

CREATE INDEX payment_orders_reconciliation_idx
    ON payment_orders (COALESCE(reconciliation_next_at, expires_at), expires_at, id)
    WHERE status IN ('CREATED', 'PENDING');
