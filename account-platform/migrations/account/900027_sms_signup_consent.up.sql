SET LOCAL lock_timeout = '2s';

-- Live Account transactions can touch these tables in either order.
LOCK TABLE sms_challenges, registration_operations IN ACCESS EXCLUSIVE MODE NOWAIT;

-- Enforce the new rules on writes immediately. Validate existing rows in
-- 900028 after these short ACCESS EXCLUSIVE locks have been released.
ALTER TABLE sms_challenges
    ADD COLUMN signup_consent_version TEXT,
    ADD COLUMN signup_login_binding BYTEA,
    ADD COLUMN signup_authorized_binding BYTEA,
    ADD CONSTRAINT sms_signup_consent_check
        CHECK (signup_consent_version IS NULL OR (purpose = 'LOGIN' AND signup_consent_version <> '')) NOT VALID,
    ADD CONSTRAINT sms_signup_login_binding_check
        CHECK ((signup_consent_version IS NULL AND signup_login_binding IS NULL) OR
               (signup_consent_version IS NOT NULL AND signup_login_binding IS NOT NULL AND octet_length(signup_login_binding) = 32)) NOT VALID,
    ADD CONSTRAINT sms_signup_authorized_binding_check
        CHECK (signup_authorized_binding IS NULL OR
               (signup_consent_version IS NOT NULL AND status='CONSUMED' AND octet_length(signup_authorized_binding) = 32)) NOT VALID;

ALTER TABLE registration_operations
    ADD COLUMN signup_consent_version TEXT,
    ADD COLUMN signup_first_dispatch_version BIGINT,
    ADD CONSTRAINT registration_signup_consent_check
        CHECK (signup_consent_version IS NULL OR signup_consent_version <> '') NOT VALID,
    ADD CONSTRAINT registration_signup_dispatch_check
        CHECK (signup_first_dispatch_version IS NULL OR (signup_first_dispatch_version > 0 AND signup_consent_version IS NOT NULL)) NOT VALID;

-- A completed registration can enter wallet repair after releasing a phone
-- through profile change. Only reservations with no committed user own a value.
-- Limit the index build itself as the replacement holds the existing table lock.
SET LOCAL statement_timeout = '2s';
DROP INDEX registration_operations_active_mobile_idx;
CREATE UNIQUE INDEX registration_operations_active_mobile_idx
    ON registration_operations (mobile_e164)
    WHERE committed_user_id IS NULL AND status IN
      ('SMS_PENDING','REGISTRATION_IN_PROGRESS','IDENTITY_CREATING','IDENTITY_CREATED','ACCOUNT_COMMITTING','REPAIR_REQUIRED','COMPENSATION_PENDING');
