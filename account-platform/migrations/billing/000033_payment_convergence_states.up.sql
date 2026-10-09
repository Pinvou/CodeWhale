ALTER TABLE payment_orders DROP CONSTRAINT payment_orders_close_status_check;
UPDATE payment_orders SET close_status='PROVIDER_CLOSED' WHERE close_status='CLOSED';
UPDATE payment_orders SET close_status='EXPIRED' WHERE close_status='FAILED';
ALTER TABLE payment_orders ADD CONSTRAINT payment_orders_close_status_check
    CHECK (close_status IN ('OPEN','CLOSE_REQUESTED','LOCALLY_CLOSED_EXTERNAL_PENDING','PROVIDER_CLOSED','EXPIRED'));
ALTER TABLE payment_orders
    ADD COLUMN close_requested_at TIMESTAMPTZ,
    ADD COLUMN locally_closed_at TIMESTAMPTZ,
    ADD COLUMN provider_closed_at TIMESTAMPTZ;
UPDATE payment_orders
SET close_requested_at=updated_at,
    locally_closed_at=updated_at,
    provider_closed_at=updated_at
WHERE close_status='PROVIDER_CLOSED';

ALTER TABLE payment_evidence ALTER COLUMN payment_order_id DROP NOT NULL;
