DROP TABLE IF EXISTS payment_order_repairs;

UPDATE payment_notifications SET result='REJECTED' WHERE result='MANUAL_REVIEW';
ALTER TABLE payment_notifications DROP CONSTRAINT IF EXISTS payment_notifications_result_check;
ALTER TABLE payment_notifications
    ADD CONSTRAINT payment_notifications_result_check
    CHECK (result IN ('RECEIVED', 'PROCESSED', 'REJECTED'));
