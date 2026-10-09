ALTER TABLE users ADD COLUMN identity_security_version BIGINT NOT NULL DEFAULT 0 CHECK (identity_security_version >= 0);

ALTER TABLE profile_change_requests
    ADD COLUMN fence_version BIGINT NOT NULL DEFAULT 0 CHECK (fence_version >= 0);

ALTER TABLE password_reset_operations
    DROP CONSTRAINT password_reset_operations_status_check,
    ADD COLUMN mobile_e164 TEXT,
    ADD COLUMN password_handle TEXT,
    ADD COLUMN password_policy_version TEXT,
    ADD COLUMN fence_version BIGINT NOT NULL DEFAULT 0 CHECK (fence_version >= 0),
    ADD COLUMN password_write_started_at TIMESTAMPTZ,
    ADD COLUMN password_write_owner UUID,
    ADD COLUMN password_updated_at TIMESTAMPTZ,
    ADD COLUMN sessions_revoked_at TIMESTAMPTZ,
    ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    ADD COLUMN next_attempt_at TIMESTAMPTZ,
    ADD COLUMN sla_expires_at TIMESTAMPTZ,
    ADD COLUMN updated_at TIMESTAMPTZ;

UPDATE password_reset_operations
SET mobile_e164='', password_handle='', password_policy_version='',
    next_attempt_at=COALESCE(completed_at,created_at),
    sla_expires_at=COALESCE(completed_at,created_at)+interval '30 minutes',
    updated_at=COALESCE(completed_at,created_at),
    password_updated_at=CASE WHEN status='COMPLETED' THEN completed_at END,
    sessions_revoked_at=CASE WHEN status='COMPLETED' THEN completed_at END;

-- Both a legacy VERIFIED row and IDENTITY_RESET_FAILED can be the durable
-- footprint of a request whose Keycloak password write committed while the
-- response/checkpoint was lost. Keep the newest ambiguous row per user as a
-- revoke-only recovery; never replay a password reference that was not
-- durably retained by the old schema.
WITH ambiguous AS (
    SELECT id,user_id,row_number() OVER (PARTITION BY user_id ORDER BY created_at DESC,id DESC) AS position
    FROM password_reset_operations
    WHERE status='VERIFIED' OR (status='FAILED' AND last_error_code='IDENTITY_RESET_FAILED')
)
UPDATE password_reset_operations operation
SET status=CASE WHEN ambiguous.position=1 THEN 'PASSWORD_STARTED' ELSE 'FAILED' END,
    password_write_started_at=CASE WHEN ambiguous.position=1 THEN COALESCE(operation.completed_at,operation.created_at) END,
    completed_at=CASE WHEN ambiguous.position=1 THEN NULL ELSE operation.completed_at END,
    last_error_code=CASE WHEN ambiguous.position=1 THEN 'LEGACY_SESSION_REVOCATION_UNCERTAIN' ELSE 'MIGRATION_SUPERSEDED' END,
    fence_version=CASE WHEN ambiguous.position=1 THEN 1 ELSE 0 END,
    next_attempt_at=operation.created_at,
    sla_expires_at=operation.created_at+interval '30 minutes',
    updated_at=COALESCE(operation.completed_at,operation.created_at)
FROM ambiguous
WHERE operation.id=ambiguous.id;

-- Existing profile operations were previously allowed one per field. Abort
-- rather than guess an order if more than one irreversible operation exists
-- for a user; an operator must converge that user before deploying this fence.
DO $$
BEGIN
    IF EXISTS (
        SELECT user_id FROM profile_change_requests
        WHERE status IN ('PENDING','VERIFIED','IDENTITY_UPDATED','ACCOUNT_COMMITTED','RETRY','REPAIR_REQUIRED')
        GROUP BY user_id HAVING count(*) > 1
    ) OR EXISTS (
        SELECT 1 FROM profile_change_requests profile
        JOIN password_reset_operations password ON password.user_id=profile.user_id
        WHERE profile.status IN ('PENDING','VERIFIED','IDENTITY_UPDATED','ACCOUNT_COMMITTED','RETRY','REPAIR_REQUIRED')
          AND password.status='PASSWORD_STARTED'
    ) THEN
        RAISE EXCEPTION '000085 requires at most one active identity-security operation per user'
            USING ERRCODE='55000';
    END IF;
END $$;

UPDATE profile_change_requests
SET fence_version=1
WHERE status IN ('PENDING','VERIFIED','IDENTITY_UPDATED','ACCOUNT_COMMITTED','RETRY','REPAIR_REQUIRED');

UPDATE users account_user
SET identity_security_version=1
WHERE EXISTS (
    SELECT 1 FROM password_reset_operations operation
    WHERE operation.user_id=account_user.id AND operation.status='PASSWORD_STARTED' AND operation.fence_version=1
)
OR EXISTS (
    SELECT 1 FROM profile_change_requests operation
    WHERE operation.user_id=account_user.id AND operation.fence_version=1
);

ALTER TABLE password_reset_operations
    ALTER COLUMN mobile_e164 SET NOT NULL,
    ALTER COLUMN password_handle SET NOT NULL,
    ALTER COLUMN password_policy_version SET NOT NULL,
    ALTER COLUMN next_attempt_at SET NOT NULL,
    ALTER COLUMN sla_expires_at SET NOT NULL,
    ALTER COLUMN updated_at SET NOT NULL,
    ADD CONSTRAINT password_reset_operations_status_check
        CHECK (status IN ('VERIFIED','PASSWORD_STARTED','PASSWORD_UPDATED','RETRY','COMPLETED','FAILED','REPAIR_REQUIRED'));

CREATE UNIQUE INDEX password_reset_operations_active_user_idx
    ON password_reset_operations(user_id)
    WHERE status IN ('VERIFIED','PASSWORD_STARTED','PASSWORD_UPDATED','RETRY','REPAIR_REQUIRED');

CREATE INDEX password_reset_operations_ready_idx
    ON password_reset_operations(next_attempt_at,updated_at)
    WHERE status IN ('VERIFIED','PASSWORD_STARTED','PASSWORD_UPDATED','RETRY');
