SET LOCAL lock_timeout = '2s';

ALTER TABLE sms_send_attempts
    VALIDATE CONSTRAINT sms_send_attempts_purpose_v2_check;
