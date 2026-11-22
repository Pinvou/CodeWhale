UPDATE payment_orders
SET status='PENDING',
    attention_summary='PROCESSING',
    summary_version=summary_version+1,
    updated_at=now()
WHERE status='EXPIRED';

DROP INDEX payment_orders_reconciliation_idx;
CREATE INDEX payment_orders_reconciliation_idx
    ON payment_orders (COALESCE(reconciliation_next_at, expires_at), expires_at, id)
    WHERE status IN ('CREATED','PENDING');

DROP INDEX payment_orders_late_boundary_idx;
CREATE INDEX payment_orders_late_boundary_idx
    ON payment_orders(late_review_boundary_at,id)
    WHERE status IN ('CREATED','PENDING') AND archive_status='ACTIVE';

ALTER TABLE payment_orders DROP CONSTRAINT payment_orders_status_check;
ALTER TABLE payment_orders ADD CONSTRAINT payment_orders_status_check
    CHECK (status IN ('CREATED','PENDING','PAID','PAID_LATE','FAILED','CLOSED','MANUAL_REVIEW'));
