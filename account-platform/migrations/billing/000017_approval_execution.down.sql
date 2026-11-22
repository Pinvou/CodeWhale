DROP TRIGGER manual_adjustments_require_approval_execution ON manual_adjustments;
DROP FUNCTION require_adjustment_approval_execution();
DROP INDEX manual_adjustments_request_idempotency_unique;
ALTER TABLE manual_adjustments
    DROP CONSTRAINT manual_adjustments_approval_execution_pair,
    DROP CONSTRAINT manual_adjustments_execution_key_unique,
    DROP CONSTRAINT manual_adjustments_approval_request_unique,
    DROP COLUMN execution_idempotency_key,
    DROP COLUMN approval_request_id,
    DROP COLUMN request_idempotency_key,
    DROP COLUMN version;
