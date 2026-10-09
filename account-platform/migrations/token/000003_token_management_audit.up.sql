ALTER TABLE token_accounts
    ADD COLUMN status_changed_by UUID,
    ADD COLUMN status_change_reason TEXT;

ALTER TABLE provider_credentials
    ADD COLUMN revoked_at TIMESTAMPTZ,
    ADD COLUMN revoked_by UUID,
    ADD COLUMN revocation_reason TEXT,
    ADD CONSTRAINT provider_credentials_revocation_audit CHECK (
        status <> 'REVOKED'
        OR (revoked_at IS NOT NULL AND revoked_by IS NOT NULL AND revocation_reason IS NOT NULL AND btrim(revocation_reason) <> '')
    );

ALTER TABLE token_share_policies ADD COLUMN updated_by UUID;
ALTER TABLE token_explicit_members ADD COLUMN created_by UUID;
ALTER TABLE routing_policies ADD COLUMN updated_by UUID;
