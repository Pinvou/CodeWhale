CREATE TABLE recharge_product_command_records (
    idempotency_key TEXT PRIMARY KEY,
    command_type TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    catalog_item_id UUID,
    actor_subject TEXT NOT NULL,
    reason TEXT NOT NULL,
    expected_version BIGINT NOT NULL,
    response_status INTEGER NOT NULL,
    response_body JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    CHECK (length(idempotency_key) BETWEEN 16 AND 128),
    CHECK (expires_at > created_at)
);

CREATE TABLE recharge_product_audits (
    audit_id UUID PRIMARY KEY,
    command_type TEXT NOT NULL,
    catalog_item_id UUID,
    actor_subject TEXT NOT NULL,
    reason TEXT NOT NULL,
    idempotency_key TEXT NOT NULL REFERENCES recharge_product_command_records(idempotency_key),
    request_hash TEXT NOT NULL,
    expected_version BIGINT NOT NULL,
    before_version BIGINT,
    after_version BIGINT,
    result TEXT NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX recharge_product_audits_item_idx
    ON recharge_product_audits(catalog_item_id,occurred_at DESC,audit_id DESC);
