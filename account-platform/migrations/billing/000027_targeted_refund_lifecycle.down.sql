DROP TABLE forced_refund_cases;
ALTER TABLE payment_evidence DROP CONSTRAINT payment_evidence_evidence_type_check;
ALTER TABLE payment_evidence ADD CONSTRAINT payment_evidence_evidence_type_check
    CHECK (evidence_type IN ('CALLBACK','QUERY','BILL','MANUAL_REPAIR','CHARGEBACK'));
UPDATE entitlement_packages SET status='FROZEN' WHERE status='REVOKED';
ALTER TABLE entitlement_packages DROP CONSTRAINT entitlement_packages_status_check;
ALTER TABLE entitlement_packages ADD CONSTRAINT entitlement_packages_status_check
    CHECK (status IN ('ACTIVE','CONSUMED','EXPIRED','CREDIT_FAILED','REPAIRED','FROZEN'));
ALTER TABLE member_quota_grants DROP CONSTRAINT member_quota_grants_suspension_reason_check;
ALTER TABLE member_quota_grants ADD CONSTRAINT member_quota_grants_suspension_reason_check
    CHECK (suspension_reason IN ('MEMBER_REMOVED','ACCOUNT_CLOSED'));
DROP TABLE refund_grant_reservations;
DROP TABLE refund_component_reservations;
DROP INDEX refunds_active_target_idx;
ALTER TABLE refunds
    DROP CONSTRAINT refunds_component_total_check,
    DROP CONSTRAINT refunds_package_transaction_fk,
    DROP CONSTRAINT refunds_payment_transaction_order_fk,
    DROP COLUMN intent_version,
    DROP COLUMN entitlement_snapshot,
    DROP COLUMN bonus_microcredits,
    DROP COLUMN base_microcredits,
    DROP COLUMN entitlement_package_id,
    DROP COLUMN payment_transaction_id;
ALTER TABLE payment_orders DROP CONSTRAINT payment_orders_status_check;
ALTER TABLE payment_orders ADD CONSTRAINT payment_orders_status_check
    CHECK (status IN ('CREATED','PENDING','PAID','PAID_LATE','FAILED','CLOSED','REFUNDED','PARTIALLY_REFUNDED','MANUAL_REVIEW'));
ALTER TABLE payment_orders DROP CONSTRAINT payment_orders_fulfillment_status_check;
ALTER TABLE payment_orders ADD CONSTRAINT payment_orders_fulfillment_status_check
    CHECK (fulfillment_status IN ('PENDING','CREDITED','REFUNDED','MANUAL_REVIEW'));
