ALTER TABLE payment_notifications DROP CONSTRAINT IF EXISTS payment_notifications_result_check;
ALTER TABLE payment_notifications
    ADD CONSTRAINT payment_notifications_result_check
    CHECK (result IN ('RECEIVED', 'PROCESSED', 'REJECTED', 'MANUAL_REVIEW'));

CREATE TABLE payment_order_repairs (
    id UUID PRIMARY KEY,
    payment_order_id UUID NOT NULL UNIQUE REFERENCES payment_orders(id),
    actor_id UUID NOT NULL,
    reason TEXT NOT NULL CHECK (length(trim(reason)) BETWEEN 1 AND 500),
    provider_snapshot JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX payment_order_repairs_actor_idx
    ON payment_order_repairs (actor_id, created_at DESC);
