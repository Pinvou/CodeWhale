SET LOCAL lock_timeout = '2s';

ALTER TABLE sms_send_attempts
    ADD CONSTRAINT sms_send_attempts_purpose_v2_check
    CHECK (purpose IN ('REGISTER', 'LOGIN', 'PROFILE_MOBILE', 'PASSWORD_RESET'))
    NOT VALID;
