SET LOCAL lock_timeout = '2s';
SET LOCAL statement_timeout = '2s';

LOCK TABLE sms_challenges, registration_operations, identities IN ACCESS EXCLUSIVE MODE NOWAIT;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM registration_operations WHERE signup_consent_version IS NOT NULL)
       OR EXISTS (SELECT 1 FROM sms_challenges WHERE signup_consent_version IS NOT NULL) THEN
        RAISE EXCEPTION 'mobile signup consent exists; retain this schema when rolling back the application';
    END IF;
END
$$;

ALTER TABLE sms_challenges
    DROP CONSTRAINT sms_signup_consent_check,
    DROP CONSTRAINT sms_signup_login_binding_check,
    DROP CONSTRAINT sms_signup_authorized_binding_check;
ALTER TABLE sms_challenges
    ADD CONSTRAINT sms_signup_consent_check
        CHECK (signup_consent_version IS NULL OR (purpose = 'LOGIN' AND signup_consent_version <> '')) NOT VALID,
    ADD CONSTRAINT sms_signup_login_binding_check
        CHECK ((signup_consent_version IS NULL AND signup_login_binding IS NULL) OR
               (signup_consent_version IS NOT NULL AND signup_login_binding IS NOT NULL AND octet_length(signup_login_binding) = 32)) NOT VALID,
    ADD CONSTRAINT sms_signup_authorized_binding_check
        CHECK (signup_authorized_binding IS NULL OR
               (signup_consent_version IS NOT NULL AND status='CONSUMED' AND octet_length(signup_authorized_binding) = 32)) NOT VALID;

ALTER TABLE registration_operations
    DROP CONSTRAINT registration_signup_consent_check,
    DROP CONSTRAINT registration_signup_dispatch_check;
ALTER TABLE registration_operations
    ADD CONSTRAINT registration_signup_consent_check
        CHECK (signup_consent_version IS NULL OR signup_consent_version <> '') NOT VALID,
    ADD CONSTRAINT registration_signup_dispatch_check
        CHECK (signup_first_dispatch_version IS NULL OR (signup_first_dispatch_version > 0 AND signup_consent_version IS NOT NULL)) NOT VALID;

DROP INDEX identities_user_issuer_idx;
