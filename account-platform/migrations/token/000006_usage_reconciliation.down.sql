DROP INDEX IF EXISTS usage_requests_reconciliation_due_idx;
DROP INDEX IF EXISTS usage_attempts_usage_request_idx;

ALTER TABLE usage_requests
    DROP COLUMN reconciliation_stopped_at,
    DROP COLUMN reconciliation_last_error,
    DROP COLUMN reconciliation_lease_until,
    DROP COLUMN reconciliation_next_at,
    DROP COLUMN reconciliation_attempts;
