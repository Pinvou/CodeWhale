CREATE INDEX slider_captcha_retention_idx ON slider_captcha_challenges(updated_at);
CREATE INDEX sms_challenges_retention_idx ON sms_challenges(updated_at);
CREATE INDEX sms_send_attempts_retention_idx ON sms_send_attempts(occurred_at) WHERE challenge_id IS NULL;
CREATE INDEX sms_verification_attempts_retention_idx ON sms_verification_attempts(occurred_at) WHERE challenge_id IS NULL;
