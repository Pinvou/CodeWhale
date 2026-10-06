-- Pending legacy partial refunds retain their original wallet reservation and
-- provider intent. Finish or review them on the preceding version before this
-- transaction replaces that command with whole-target component reservations.
LOCK TABLE refunds IN ACCESS EXCLUSIVE MODE;
DO $$ BEGIN
    IF EXISTS(SELECT 1 FROM refunds WHERE status IN ('REQUESTED','MANUAL_REVIEW')) THEN
        RAISE EXCEPTION 'finish and verify pending legacy refunds on the preceding version before migration 27';
    END IF;
END $$;

ALTER TABLE journal_entries DROP CONSTRAINT journal_entries_entry_type_check;
ALTER TABLE journal_entries ADD CONSTRAINT journal_entries_entry_type_check
    CHECK (entry_type IN ('RECHARGE','USAGE','REFUND','REVERSAL','ADJUSTMENT','EXPIRE','REPAIR_CREDIT','RECEIVABLE','CHARGEBACK','FORCED_REFUND'));
ALTER TABLE receivables DROP CONSTRAINT receivables_source_type_check;
ALTER TABLE receivables ADD CONSTRAINT receivables_source_type_check
    CHECK (source_type IN ('USAGE_OVERAGE','CHARGEBACK','FORCED_REFUND'));

ALTER TABLE refunds
    ADD COLUMN payment_transaction_id UUID,
    ADD COLUMN entitlement_package_id UUID,
    ADD COLUMN base_microcredits BIGINT NOT NULL DEFAULT 0 CHECK (base_microcredits >= 0),
    ADD COLUMN bonus_microcredits BIGINT NOT NULL DEFAULT 0 CHECK (bonus_microcredits >= 0),
    ADD COLUMN entitlement_snapshot JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN intent_version BIGINT NOT NULL DEFAULT 1 CHECK (intent_version > 0);

-- Payment success remains the order fact. Refund state is projected only via
-- the independent refund summary and must never replace the payment status.
ALTER TABLE payment_orders DROP CONSTRAINT payment_orders_status_check;
UPDATE payment_orders payment SET status=CASE
    WHEN EXISTS(SELECT 1 FROM payment_transactions transaction WHERE transaction.payment_order_id=payment.id AND transaction.status='LATE_PAID') THEN 'PAID_LATE'
    ELSE 'PAID'
END WHERE payment.status IN ('PARTIALLY_REFUNDED','REFUNDED');
ALTER TABLE payment_orders ADD CONSTRAINT payment_orders_status_check
    CHECK (status IN ('CREATED','PENDING','PAID','PAID_LATE','FAILED','CLOSED','MANUAL_REVIEW'));
ALTER TABLE payment_orders DROP CONSTRAINT payment_orders_fulfillment_status_check;
UPDATE payment_orders SET fulfillment_status='CREDITED' WHERE fulfillment_status='REFUNDED';
ALTER TABLE payment_orders ADD CONSTRAINT payment_orders_fulfillment_status_check
    CHECK (fulfillment_status IN ('PENDING','CREDITED','MANUAL_REVIEW'));

WITH unique_targets AS (
    SELECT refund.id AS refund_id,
           (array_agg(package.id ORDER BY package.credited_at,package.id))[1] AS package_id,
           (array_agg(package.source_payment_transaction_id ORDER BY package.credited_at,package.id))[1] AS transaction_id
    FROM refunds refund
    JOIN entitlement_packages package ON package.payment_order_id=refund.payment_order_id
    WHERE package.source_payment_transaction_id IS NOT NULL
    GROUP BY refund.id
    HAVING count(*)=1
)
UPDATE refunds refund SET
    payment_transaction_id=target.transaction_id,
    entitlement_package_id=target.package_id,
    base_microcredits=refund.credit_microcredits,
    entitlement_snapshot=jsonb_build_object('migration_source','LEGACY_ORDER_REFUND')
FROM unique_targets target
WHERE target.refund_id=refund.id;

DO $$ BEGIN
    IF EXISTS(SELECT 1 FROM refunds WHERE payment_transaction_id IS NULL OR entitlement_package_id IS NULL) THEN
        RAISE EXCEPTION 'legacy refunds require an unambiguous payment transaction and entitlement package';
    END IF;
END $$;

ALTER TABLE refunds
    ALTER COLUMN payment_transaction_id SET NOT NULL,
    ALTER COLUMN entitlement_package_id SET NOT NULL,
    ADD CONSTRAINT refunds_payment_transaction_order_fk FOREIGN KEY(payment_transaction_id,payment_order_id)
        REFERENCES payment_transactions(id,payment_order_id),
    ADD CONSTRAINT refunds_package_transaction_fk FOREIGN KEY(entitlement_package_id,payment_transaction_id)
        REFERENCES entitlement_packages(id,source_payment_transaction_id),
    ADD CONSTRAINT refunds_component_total_check
        CHECK (base_microcredits>0 AND base_microcredits+bonus_microcredits=credit_microcredits);

CREATE UNIQUE INDEX refunds_active_target_idx ON refunds(payment_transaction_id,entitlement_package_id)
    WHERE status IN ('REQUESTED','MANUAL_REVIEW');

CREATE TABLE refund_component_reservations (
    refund_id UUID NOT NULL REFERENCES refunds(id),
    component_id UUID NOT NULL REFERENCES entitlement_components(id),
    package_id UUID NOT NULL REFERENCES entitlement_packages(id),
    component_type TEXT NOT NULL CHECK (component_type IN ('BASE','BONUS')),
    credit_lot_id UUID NOT NULL REFERENCES credit_lots(id),
    available_microcredits BIGINT NOT NULL CHECK (available_microcredits >= 0),
    active_grant_microcredits BIGINT NOT NULL CHECK (active_grant_microcredits >= 0),
    suspended_grant_microcredits BIGINT NOT NULL CHECK (suspended_grant_microcredits >= 0),
    prior_status TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY(refund_id,component_id),
    FOREIGN KEY(component_id,package_id,component_type) REFERENCES entitlement_components(id,package_id,component_type)
);

CREATE TABLE refund_grant_reservations (
    refund_id UUID NOT NULL REFERENCES refunds(id),
    grant_id UUID NOT NULL REFERENCES member_quota_grants(id),
    component_id UUID NOT NULL REFERENCES entitlement_components(id),
    available_microcredits BIGINT NOT NULL CHECK (available_microcredits >= 0),
    prior_status TEXT NOT NULL,
    prior_suspension_reason TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY(refund_id,grant_id)
);

ALTER TABLE member_quota_grants DROP CONSTRAINT member_quota_grants_suspension_reason_check;
ALTER TABLE member_quota_grants ADD CONSTRAINT member_quota_grants_suspension_reason_check
    CHECK (suspension_reason IN ('MEMBER_REMOVED','ACCOUNT_CLOSED','CHARGEBACK_REVIEW','REFUND_PENDING','FORCED_REFUND_REVIEW'));

ALTER TABLE entitlement_packages DROP CONSTRAINT entitlement_packages_status_check;
ALTER TABLE entitlement_packages ADD CONSTRAINT entitlement_packages_status_check
    CHECK (status IN ('ACTIVE','CONSUMED','EXPIRED','CREDIT_FAILED','REPAIRED','FROZEN','REVOKED'));

ALTER TABLE payment_evidence DROP CONSTRAINT payment_evidence_evidence_type_check;
ALTER TABLE payment_evidence ADD CONSTRAINT payment_evidence_evidence_type_check
    CHECK (evidence_type IN ('CALLBACK','QUERY','BILL','MANUAL_REPAIR','CHARGEBACK','FORCED_REFUND'));

CREATE TABLE forced_refund_cases (
    id UUID PRIMARY KEY,
    payment_evidence_id UUID NOT NULL UNIQUE REFERENCES payment_evidence(id),
    payment_order_id UUID NOT NULL REFERENCES payment_orders(id),
    payment_transaction_id UUID NOT NULL REFERENCES payment_transactions(id),
    entitlement_package_id UUID NOT NULL REFERENCES entitlement_packages(id),
    wallet_id UUID NOT NULL REFERENCES wallets(id),
    provider_refund_id TEXT NOT NULL,
    amount_fen BIGINT NOT NULL CHECK (amount_fen > 0),
    status TEXT NOT NULL CHECK (status IN ('REVIEW_REQUIRED','RESOLVED')),
    entitlement_snapshot JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    resolved_at TIMESTAMPTZ,
    FOREIGN KEY(payment_transaction_id,payment_order_id) REFERENCES payment_transactions(id,payment_order_id),
    FOREIGN KEY(entitlement_package_id,payment_transaction_id) REFERENCES entitlement_packages(id,source_payment_transaction_id),
    UNIQUE(payment_transaction_id,provider_refund_id)
);

CREATE INDEX forced_refund_cases_review_idx ON forced_refund_cases(created_at,id)
    WHERE status='REVIEW_REQUIRED';
