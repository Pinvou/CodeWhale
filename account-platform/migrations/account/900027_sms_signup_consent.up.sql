ALTER TABLE sms_challenges ADD COLUMN signup_consent_version TEXT
    CHECK (signup_consent_version IS NULL OR (purpose = 'LOGIN' AND signup_consent_version <> ''));

ALTER TABLE sms_challenges ADD COLUMN signup_login_binding BYTEA
    CHECK ((signup_consent_version IS NULL AND signup_login_binding IS NULL) OR
           (signup_consent_version IS NOT NULL AND signup_login_binding IS NOT NULL AND octet_length(signup_login_binding) = 32));
ALTER TABLE sms_challenges ADD COLUMN signup_authorized_binding BYTEA
    CHECK (signup_authorized_binding IS NULL OR
           (signup_consent_version IS NOT NULL AND status='CONSUMED' AND octet_length(signup_authorized_binding) = 32));

ALTER TABLE registration_operations ADD COLUMN signup_consent_version TEXT
    CHECK (signup_consent_version IS NULL OR signup_consent_version <> '');

ALTER TABLE registration_operations ADD COLUMN signup_first_dispatch_version BIGINT
    CHECK (signup_first_dispatch_version IS NULL OR (signup_first_dispatch_version > 0 AND signup_consent_version IS NOT NULL));

-- A completed registration can enter wallet repair after releasing a phone
-- through profile change. Only reservations with no committed user own a value.
DROP INDEX registration_operations_active_mobile_idx;
CREATE UNIQUE INDEX registration_operations_active_mobile_idx
    ON registration_operations (mobile_e164)
    WHERE committed_user_id IS NULL AND status IN
      ('SMS_PENDING','REGISTRATION_IN_PROGRESS','IDENTITY_CREATING','IDENTITY_CREATED','ACCOUNT_COMMITTING','REPAIR_REQUIRED','COMPENSATION_PENDING');
