ALTER TABLE routing_policies DROP COLUMN updated_by;
ALTER TABLE token_explicit_members DROP COLUMN created_by;
ALTER TABLE token_share_policies DROP COLUMN updated_by;

ALTER TABLE provider_credentials
    DROP CONSTRAINT provider_credentials_revocation_audit,
    DROP COLUMN revocation_reason,
    DROP COLUMN revoked_by,
    DROP COLUMN revoked_at;

ALTER TABLE token_accounts
    DROP COLUMN status_change_reason,
    DROP COLUMN status_changed_by;
