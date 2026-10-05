DELETE FROM role_permissions WHERE permission_code = 'platform.registrations.repair';
DELETE FROM permissions WHERE code = 'platform.registrations.repair';

DROP TABLE wallet_provisioning_jobs;
ALTER TABLE registration_operations DROP CONSTRAINT registration_operations_sms_challenge_fk;
DROP INDEX sms_challenges_registration_operation_idx;
ALTER TABLE sms_challenges DROP COLUMN registration_operation_id;
DROP TABLE registration_operations;

UPDATE users SET status = 'PROVISIONING' WHERE status = 'REPAIR_REQUIRED';
ALTER TABLE users DROP CONSTRAINT users_status_check;
ALTER TABLE users ADD CONSTRAINT users_status_check
    CHECK (status IN ('PROVISIONING', 'ACTIVE', 'DISABLED', 'DELETED'));
