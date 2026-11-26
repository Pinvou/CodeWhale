SET LOCAL lock_timeout = '2s';

ALTER TABLE sms_challenges
    VALIDATE CONSTRAINT sms_signup_consent_check,
    VALIDATE CONSTRAINT sms_signup_login_binding_check,
    VALIDATE CONSTRAINT sms_signup_authorized_binding_check;

ALTER TABLE registration_operations
    VALIDATE CONSTRAINT registration_signup_consent_check,
    VALIDATE CONSTRAINT registration_signup_dispatch_check;

-- The SMS login binding joins a user to identities for a particular issuer.
-- Keep a short build budget for the deployed small identities table.
SET LOCAL statement_timeout = '2s';
CREATE INDEX identities_user_issuer_idx ON identities (user_id, issuer);
