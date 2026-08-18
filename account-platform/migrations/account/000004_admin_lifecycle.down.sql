UPDATE users SET status = 'DISABLED' WHERE status = 'DELETED';
UPDATE organizations SET status = 'DISABLED' WHERE status = 'DELETED';

ALTER TABLE users DROP CONSTRAINT users_status_check;
ALTER TABLE users
    ADD CONSTRAINT users_status_check
    CHECK (status IN ('PROVISIONING', 'ACTIVE', 'DISABLED'));

ALTER TABLE organizations DROP CONSTRAINT organizations_status_check;
ALTER TABLE organizations
    ADD CONSTRAINT organizations_status_check
    CHECK (status IN ('ACTIVE', 'DISABLED'));
