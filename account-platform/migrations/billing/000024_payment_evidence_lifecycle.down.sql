ALTER TABLE payment_orders
    DROP COLUMN summary_version,
    DROP COLUMN attention_summary,
    DROP COLUMN chargeback_summary,
    DROP COLUMN refund_summary,
    DROP COLUMN payment_summary,
    DROP COLUMN close_status,
    DROP COLUMN payer_commerce_mapping_snapshot,
    DROP COLUMN payer_user_id;

ALTER TABLE payment_order_repairs
    DROP CONSTRAINT payment_order_repairs_transaction_order_fk,
    DROP CONSTRAINT payment_order_repairs_payment_transaction_id_key,
    DROP COLUMN payment_evidence_id,
    DROP COLUMN payment_transaction_id,
    ADD CONSTRAINT payment_order_repairs_payment_order_id_key UNIQUE(payment_order_id);

DROP INDEX entitlement_packages_payment_order_idx;
DROP INDEX entitlement_packages_payment_transaction_idx;
ALTER TABLE entitlement_packages
    DROP CONSTRAINT entitlement_packages_payment_source_order_fk,
    DROP CONSTRAINT entitlement_packages_payment_source_check,
    DROP COLUMN source_payment_transaction_id,
    ADD CONSTRAINT entitlement_packages_payment_order_id_key UNIQUE(payment_order_id);

ALTER TABLE payment_notifications DROP COLUMN payment_evidence_id;
DROP TRIGGER payment_evidence_immutable ON payment_evidence;
DROP FUNCTION reject_payment_evidence_mutation();
DROP TABLE payment_evidence;

DROP INDEX payment_transactions_review_idx;
DROP INDEX payment_transactions_order_idx;
ALTER TABLE payment_transactions DROP CONSTRAINT payment_transactions_order_identity_unique;
ALTER TABLE payment_transactions
    DROP CONSTRAINT payment_transactions_fulfillment_check,
    DROP CONSTRAINT payment_transactions_source_check,
    DROP CONSTRAINT payment_transactions_status_check,
    DROP COLUMN evidence_version,
    DROP COLUMN fulfilled_at,
    DROP COLUMN fulfillment_status,
    DROP COLUMN provider_paid_at,
    DROP COLUMN provider_status,
    DROP COLUMN provider_version,
    DROP COLUMN provider_id;
UPDATE payment_transactions SET status='SUCCEEDED' WHERE status IN ('PAID','LATE_PAID');
UPDATE payment_transactions SET status='MANUAL_REVIEW' WHERE status='REVIEW_REQUIRED';
UPDATE payment_transactions SET source='NOTIFICATION' WHERE source='CALLBACK';
UPDATE payment_transactions SET source='RECONCILIATION' WHERE source IN ('BILL','MANUAL_REPAIR');
ALTER TABLE payment_transactions ADD CONSTRAINT payment_transactions_status_check
    CHECK (status IN ('SUCCEEDED','REFUNDED','PARTIALLY_REFUNDED','MANUAL_REVIEW'));
ALTER TABLE payment_transactions ADD CONSTRAINT payment_transactions_source_check
    CHECK (source IN ('NOTIFICATION','QUERY','RECONCILIATION'));
CREATE UNIQUE INDEX payment_transactions_order_idx ON payment_transactions(payment_order_id);

ALTER TABLE payment_transactions DROP CONSTRAINT payment_transactions_channel_check;
ALTER TABLE payment_transactions ADD CONSTRAINT payment_transactions_channel_check
    CHECK (channel IN ('UNIONPAY','ALIPAY','WECHAT'));
ALTER TABLE payment_orders DROP CONSTRAINT payment_orders_channel_check;
ALTER TABLE payment_orders ADD CONSTRAINT payment_orders_channel_check
    CHECK (channel IN ('UNIONPAY','ALIPAY','WECHAT'));

ALTER TABLE payment_transactions RENAME COLUMN provider_transaction_id TO channel_transaction_id;
ALTER TABLE payment_orders RENAME COLUMN primary_provider_transaction_id TO channel_transaction_id;
