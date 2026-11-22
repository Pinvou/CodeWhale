DROP INDEX password_reset_operations_ready_idx;
DROP INDEX password_reset_operations_active_user_idx;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM password_reset_operations
        WHERE status IN ('VERIFIED','PASSWORD_STARTED','PASSWORD_UPDATED','RETRY','REPAIR_REQUIRED')
    ) THEN
        RAISE EXCEPTION 'cannot roll back password-reset recovery while nonterminal operations exist';
    END IF;
END $$;

ALTER TABLE password_reset_operations
    DROP CONSTRAINT password_reset_operations_status_check,
    ADD CONSTRAINT password_reset_operations_status_check CHECK (status IN ('VERIFIED','COMPLETED','FAILED')),
    DROP COLUMN mobile_e164,
    DROP COLUMN password_handle,
    DROP COLUMN password_policy_version,
    DROP COLUMN fence_version,
    DROP COLUMN password_write_started_at,
    DROP COLUMN password_write_owner,
    DROP COLUMN password_updated_at,
    DROP COLUMN sessions_revoked_at,
    DROP COLUMN attempts,
    DROP COLUMN next_attempt_at,
    DROP COLUMN sla_expires_at,
    DROP COLUMN updated_at;

ALTER TABLE profile_change_requests DROP COLUMN fence_version;
ALTER TABLE users DROP COLUMN identity_security_version;
