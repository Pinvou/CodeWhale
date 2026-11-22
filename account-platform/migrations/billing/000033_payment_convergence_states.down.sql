DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM payment_evidence WHERE payment_order_id IS NULL) THEN
        RAISE EXCEPTION 'cannot downgrade while unmatched provider bill evidence exists';
    END IF;
END $$;
ALTER TABLE payment_evidence ALTER COLUMN payment_order_id SET NOT NULL;
ALTER TABLE payment_orders
    DROP COLUMN provider_closed_at,
    DROP COLUMN locally_closed_at,
    DROP COLUMN close_requested_at;
ALTER TABLE payment_orders DROP CONSTRAINT payment_orders_close_status_check;
UPDATE payment_orders SET close_status='CLOSED' WHERE close_status='PROVIDER_CLOSED';
UPDATE payment_orders SET close_status='FAILED' WHERE close_status='EXPIRED';
ALTER TABLE payment_orders ADD CONSTRAINT payment_orders_close_status_check
    CHECK (close_status IN ('OPEN','CLOSE_REQUESTED','LOCALLY_CLOSED_EXTERNAL_PENDING','CLOSED','FAILED'));
