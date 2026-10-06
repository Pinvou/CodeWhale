-- Payment orders are commercial intents.  Provider money movements, evidence,
-- refunds and chargebacks have independent lifecycles and only project summaries
-- back to the order.
DROP INDEX payment_transactions_order_idx;

ALTER TABLE payment_orders RENAME COLUMN channel_transaction_id TO primary_provider_transaction_id;
ALTER TABLE payment_transactions RENAME COLUMN channel_transaction_id TO provider_transaction_id;

ALTER TABLE payment_orders DROP CONSTRAINT payment_orders_channel_check;
ALTER TABLE payment_orders ADD CONSTRAINT payment_orders_channel_check
    CHECK (channel ~ '^[A-Z0-9_]{1,32}$');
ALTER TABLE payment_transactions DROP CONSTRAINT payment_transactions_channel_check;
ALTER TABLE payment_transactions ADD CONSTRAINT payment_transactions_channel_check
    CHECK (channel ~ '^[A-Z0-9_]{1,32}$');

ALTER TABLE payment_transactions DROP CONSTRAINT payment_transactions_status_check;
UPDATE payment_transactions transaction
SET status=CASE
        WHEN transaction.status='MANUAL_REVIEW' THEN 'REVIEW_REQUIRED'
        WHEN payment.expires_at <= transaction.verified_at THEN 'LATE_PAID'
        ELSE 'PAID'
    END
FROM payment_orders payment
WHERE payment.id=transaction.payment_order_id;
ALTER TABLE payment_transactions DROP CONSTRAINT payment_transactions_source_check;
UPDATE payment_transactions SET source='CALLBACK' WHERE source='NOTIFICATION';
UPDATE payment_transactions SET source='BILL' WHERE source='RECONCILIATION';
ALTER TABLE payment_transactions
    ADD COLUMN provider_id TEXT NOT NULL DEFAULT 'legacy',
    ADD COLUMN provider_version TEXT NOT NULL DEFAULT 'legacy-v1',
    ADD COLUMN provider_status TEXT,
    ADD COLUMN provider_paid_at TIMESTAMPTZ,
    ADD COLUMN fulfillment_status TEXT NOT NULL DEFAULT 'PENDING'
        CHECK (fulfillment_status IN ('PENDING','CREDITED','REVIEW_REQUIRED')),
    ADD COLUMN fulfilled_at TIMESTAMPTZ,
    ADD COLUMN evidence_version BIGINT NOT NULL DEFAULT 1 CHECK (evidence_version > 0),
    ADD CONSTRAINT payment_transactions_status_check
        CHECK (status IN ('PAID','LATE_PAID','REVIEW_REQUIRED')),
    ADD CONSTRAINT payment_transactions_source_check
        CHECK (source IN ('CALLBACK','QUERY','BILL','MANUAL_REPAIR')),
    ADD CONSTRAINT payment_transactions_fulfillment_check
        CHECK ((fulfillment_status='CREDITED') = (fulfilled_at IS NOT NULL));
ALTER TABLE payment_transactions ADD CONSTRAINT payment_transactions_order_identity_unique
    UNIQUE(id,payment_order_id);

UPDATE payment_transactions transaction
SET provider_id=payment.provider_id,
    provider_version=payment.provider_version,
    provider_paid_at=payment.paid_at,
    fulfillment_status=CASE WHEN payment.fulfillment_status='CREDITED' THEN 'CREDITED' ELSE 'REVIEW_REQUIRED' END,
    fulfilled_at=CASE WHEN payment.fulfillment_status='CREDITED' THEN transaction.verified_at ELSE NULL END
FROM payment_orders payment
WHERE payment.id=transaction.payment_order_id;

CREATE INDEX payment_transactions_order_idx
    ON payment_transactions(payment_order_id,created_at,id);
CREATE INDEX payment_transactions_review_idx
    ON payment_transactions(updated_at,id)
    WHERE status='REVIEW_REQUIRED' OR fulfillment_status='REVIEW_REQUIRED';

CREATE TABLE payment_evidence (
    id UUID PRIMARY KEY,
    payment_order_id UUID NOT NULL REFERENCES payment_orders(id),
    payment_transaction_id UUID,
    evidence_type TEXT NOT NULL CHECK (evidence_type IN ('CALLBACK','QUERY','BILL','MANUAL_REPAIR')),
    provider_id TEXT NOT NULL,
    provider_version TEXT NOT NULL,
    evidence_key TEXT NOT NULL,
    merchant_order_no TEXT NOT NULL,
    provider_transaction_id TEXT,
    provider_refund_id TEXT,
    amount_fen BIGINT CHECK (amount_fen IS NULL OR amount_fen > 0),
    currency TEXT,
    provider_status TEXT,
    provider_event_at TIMESTAMPTZ,
    received_at TIMESTAMPTZ NOT NULL,
    payload_digest TEXT NOT NULL CHECK (payload_digest ~ '^[0-9a-f]{64}$'),
    payload_snapshot JSONB NOT NULL,
    merge_result TEXT NOT NULL CHECK (merge_result IN ('RECORDED','DUPLICATE','MERGED','REVIEW_REQUIRED')),
    FOREIGN KEY(payment_transaction_id,payment_order_id)
        REFERENCES payment_transactions(id,payment_order_id),
    UNIQUE(provider_id,provider_version,evidence_type,evidence_key,payload_digest)
);

CREATE INDEX payment_evidence_order_idx
    ON payment_evidence(payment_order_id,received_at,id);
CREATE INDEX payment_evidence_transaction_idx
    ON payment_evidence(payment_transaction_id,received_at,id)
    WHERE payment_transaction_id IS NOT NULL;

CREATE OR REPLACE FUNCTION reject_payment_evidence_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'payment evidence is immutable';
END $$;
CREATE TRIGGER payment_evidence_immutable BEFORE UPDATE OR DELETE ON payment_evidence
FOR EACH ROW EXECUTE FUNCTION reject_payment_evidence_mutation();

ALTER TABLE payment_notifications ADD COLUMN payment_evidence_id UUID REFERENCES payment_evidence(id);

ALTER TABLE entitlement_packages
    DROP CONSTRAINT entitlement_packages_payment_order_id_key,
    ADD COLUMN source_payment_transaction_id UUID REFERENCES payment_transactions(id);
UPDATE entitlement_packages package
SET source_payment_transaction_id=transaction.id
FROM payment_transactions transaction
WHERE package.payment_order_id=transaction.payment_order_id AND package.source_type='PAYMENT';
ALTER TABLE entitlement_packages ADD CONSTRAINT entitlement_packages_payment_source_check
    CHECK (source_type<>'PAYMENT' OR source_payment_transaction_id IS NOT NULL);
ALTER TABLE entitlement_packages ADD CONSTRAINT entitlement_packages_payment_source_order_fk
    FOREIGN KEY(source_payment_transaction_id,payment_order_id)
    REFERENCES payment_transactions(id,payment_order_id);
CREATE UNIQUE INDEX entitlement_packages_payment_transaction_idx
    ON entitlement_packages(source_payment_transaction_id)
    WHERE source_payment_transaction_id IS NOT NULL;
CREATE INDEX entitlement_packages_payment_order_idx
    ON entitlement_packages(payment_order_id,credited_at,id)
    WHERE payment_order_id IS NOT NULL;

ALTER TABLE payment_order_repairs
    DROP CONSTRAINT payment_order_repairs_payment_order_id_key,
    ADD COLUMN payment_transaction_id UUID REFERENCES payment_transactions(id),
    ADD COLUMN payment_evidence_id UUID REFERENCES payment_evidence(id);
UPDATE payment_order_repairs repair
SET payment_transaction_id=transaction.id
FROM payment_transactions transaction
WHERE transaction.payment_order_id=repair.payment_order_id;
ALTER TABLE payment_order_repairs
    ALTER COLUMN payment_transaction_id SET NOT NULL,
    ADD CONSTRAINT payment_order_repairs_payment_transaction_id_key UNIQUE(payment_transaction_id),
    ADD CONSTRAINT payment_order_repairs_transaction_order_fk
        FOREIGN KEY(payment_transaction_id,payment_order_id)
        REFERENCES payment_transactions(id,payment_order_id);

ALTER TABLE payment_orders
    ADD COLUMN payer_user_id UUID,
    ADD COLUMN payer_commerce_mapping_snapshot JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN close_status TEXT NOT NULL DEFAULT 'OPEN'
        CHECK (close_status IN ('OPEN','CLOSE_REQUESTED','LOCALLY_CLOSED_EXTERNAL_PENDING','CLOSED','FAILED')),
    ADD COLUMN payment_summary JSONB NOT NULL DEFAULT '{"paid_count":0,"paid_amount_fen":"0","late_paid_count":0,"review_count":0}'::jsonb,
    ADD COLUMN refund_summary JSONB NOT NULL DEFAULT '{"succeeded_amount_fen":"0","pending_count":0,"review_count":0}'::jsonb,
    ADD COLUMN chargeback_summary JSONB NOT NULL DEFAULT '{"open_count":0,"lost_amount_fen":"0"}'::jsonb,
    ADD COLUMN attention_summary TEXT NOT NULL DEFAULT 'NORMAL'
        CHECK (attention_summary IN ('NORMAL','PROCESSING','REVIEW_REQUIRED','RECEIVABLE_OPEN')),
    ADD COLUMN summary_version BIGINT NOT NULL DEFAULT 1 CHECK (summary_version > 0);
UPDATE payment_orders SET payer_user_id=requester_id;
ALTER TABLE payment_orders ALTER COLUMN payer_user_id SET NOT NULL;

UPDATE payment_orders payment
SET payment_summary=jsonb_build_object(
        'paid_count', facts.paid_count,
        'paid_amount_fen', facts.paid_amount_fen::text,
        'late_paid_count', facts.late_paid_count,
        'review_count', facts.review_count),
    attention_summary=CASE WHEN facts.review_count>0 THEN 'REVIEW_REQUIRED' ELSE 'NORMAL' END
FROM (
    SELECT payment_order_id,
           count(*) FILTER (WHERE status IN ('PAID','LATE_PAID')) paid_count,
           COALESCE(sum(amount_fen) FILTER (WHERE status IN ('PAID','LATE_PAID')),0) paid_amount_fen,
           count(*) FILTER (WHERE status='LATE_PAID') late_paid_count,
           count(*) FILTER (WHERE status='REVIEW_REQUIRED' OR fulfillment_status='REVIEW_REQUIRED') review_count
    FROM payment_transactions GROUP BY payment_order_id
) facts
WHERE payment.id=facts.payment_order_id;

UPDATE payment_orders payment
SET refund_summary=jsonb_build_object(
    'succeeded_amount_fen', facts.succeeded_amount_fen::text,
    'pending_count', facts.pending_count,
    'review_count', facts.review_count)
FROM (
    SELECT payment_order_id,
           COALESCE(sum(amount_fen) FILTER (WHERE status='SUCCEEDED'),0) succeeded_amount_fen,
           count(*) FILTER (WHERE status='REQUESTED') pending_count,
           count(*) FILTER (WHERE status='MANUAL_REVIEW') review_count
    FROM refunds GROUP BY payment_order_id
) facts
WHERE payment.id=facts.payment_order_id;
