DROP TABLE sms_verification_attempts;
DROP INDEX sms_send_attempts_purpose_mobile_idx;
DROP INDEX sms_send_attempts_provider_idx;
ALTER TABLE sms_send_attempts DROP COLUMN adapter_version, DROP COLUMN provider_id;
