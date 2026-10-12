ALTER TABLE payment_orders
    ADD COLUMN mobile_e164 TEXT,
    ADD COLUMN provider_sku TEXT,
    ADD COLUMN qr_data TEXT,
    ADD COLUMN provider_status TEXT,
    ADD COLUMN provider_trace_id TEXT;

ALTER TABLE payment_orders
    ADD CONSTRAINT payment_orders_qr_data_size CHECK (qr_data IS NULL OR octet_length(qr_data) <= 1048576);

CREATE INDEX payment_orders_provider_status_idx
    ON payment_orders (provider_status, updated_at DESC)
    WHERE provider_status IS NOT NULL;
