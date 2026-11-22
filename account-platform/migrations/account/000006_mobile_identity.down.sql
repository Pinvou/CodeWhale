DROP TABLE mall_user_sync_jobs;
DROP TABLE sms_challenges;
DROP INDEX users_mobile_e164_unique_idx;
ALTER TABLE users DROP COLUMN mobile_verified_at, DROP COLUMN mobile_e164;
