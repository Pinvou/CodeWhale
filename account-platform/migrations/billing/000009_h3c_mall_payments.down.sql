DROP INDEX payment_orders_provider_status_idx;
ALTER TABLE payment_orders DROP CONSTRAINT payment_orders_qr_data_size;
ALTER TABLE payment_orders
    DROP COLUMN provider_trace_id,
    DROP COLUMN provider_status,
    DROP COLUMN qr_data,
    DROP COLUMN provider_sku,
    DROP COLUMN mobile_e164;
