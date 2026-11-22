-- The Pinvou payment order owns the user-visible payment window. Provider
-- order closure remains an independent observation and is not required for
-- the local order to become expired.
ALTER TABLE payment_orders DROP CONSTRAINT payment_orders_status_check;
ALTER TABLE payment_orders ADD CONSTRAINT payment_orders_status_check
    CHECK (status IN ('CREATED','PENDING','PAID','PAID_LATE','FAILED','CLOSED','EXPIRED','MANUAL_REVIEW'));

UPDATE payment_orders
SET status='EXPIRED',
    attention_summary='NORMAL',
    payment_url=NULL,
    qr_data=NULL,
    summary_version=summary_version+1,
    updated_at=now()
WHERE status IN ('CREATED','PENDING')
  AND close_status IN ('CLOSE_REQUESTED','LOCALLY_CLOSED_EXTERNAL_PENDING')
  AND expires_at<=now();

DROP INDEX payment_orders_reconciliation_idx;
CREATE INDEX payment_orders_reconciliation_idx
    ON payment_orders (COALESCE(reconciliation_next_at, expires_at), expires_at, id)
    WHERE status IN ('CREATED','PENDING','EXPIRED');

DROP INDEX payment_orders_late_boundary_idx;
CREATE INDEX payment_orders_late_boundary_idx
    ON payment_orders(late_review_boundary_at,id)
    WHERE status IN ('CREATED','PENDING','EXPIRED') AND archive_status='ACTIVE';
