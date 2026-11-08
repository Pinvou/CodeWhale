DROP INDEX IF EXISTS payment_notifications_retry_idx;

ALTER TABLE payment_notifications
    DROP COLUMN IF EXISTS last_error_code,
    DROP COLUMN IF EXISTS next_attempt_at,
    DROP COLUMN IF EXISTS last_attempt_at,
    DROP COLUMN IF EXISTS processing_attempts;

-- The repaired system accounts are foundational financial facts and may be
-- referenced immediately after the migration. A rollback must not delete them.
