SET LOCAL lock_timeout = '2s';
SET LOCAL statement_timeout = '2s';

LOCK TABLE sms_challenges, registration_operations IN ACCESS EXCLUSIVE MODE NOWAIT;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM registration_operations WHERE signup_consent_version IS NOT NULL)
       OR EXISTS (SELECT 1 FROM sms_challenges WHERE signup_consent_version IS NOT NULL) THEN
        RAISE EXCEPTION 'mobile signup consent exists; retain this schema when rolling back the application';
    END IF;
END
$$;

DROP INDEX registration_operations_active_mobile_idx;
CREATE UNIQUE INDEX registration_operations_active_mobile_idx
    ON registration_operations (mobile_e164)
    WHERE status IN
      ('SMS_PENDING','REGISTRATION_IN_PROGRESS','IDENTITY_CREATING','IDENTITY_CREATED','ACCOUNT_COMMITTING','REPAIR_REQUIRED','COMPENSATION_PENDING');
ALTER TABLE sms_challenges DROP COLUMN signup_authorized_binding, DROP COLUMN signup_login_binding, DROP COLUMN signup_consent_version;
ALTER TABLE registration_operations DROP COLUMN signup_first_dispatch_version, DROP COLUMN signup_consent_version;
