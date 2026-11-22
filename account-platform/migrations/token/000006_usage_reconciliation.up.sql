ALTER TABLE usage_requests
    ADD COLUMN reconciliation_attempts INTEGER NOT NULL DEFAULT 0 CHECK (reconciliation_attempts >= 0),
    ADD COLUMN reconciliation_next_at TIMESTAMPTZ,
    ADD COLUMN reconciliation_lease_until TIMESTAMPTZ,
    ADD COLUMN reconciliation_last_error TEXT,
    ADD COLUMN reconciliation_stopped_at TIMESTAMPTZ;

CREATE INDEX usage_requests_reconciliation_due_idx
    ON usage_requests (reconciliation_next_at, updated_at)
    WHERE status IN ('SETTLEMENT_PENDING', 'REVIEW')
      AND reconciliation_stopped_at IS NULL;

CREATE UNIQUE INDEX usage_attempts_usage_request_idx
    ON usage_attempts (usage_request_id);
