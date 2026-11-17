CREATE INDEX sms_send_attempts_accepted_provider_window_idx
    ON sms_send_attempts(provider_id,occurred_at DESC) WHERE result='ACCEPTED';

CREATE INDEX sms_verification_attempts_accepted_provider_window_idx
    ON sms_verification_attempts(provider_id,occurred_at DESC) WHERE result='ACCEPTED';
