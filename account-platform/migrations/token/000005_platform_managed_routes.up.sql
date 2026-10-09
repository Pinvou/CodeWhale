ALTER TABLE token_accounts
    ADD COLUMN management_mode TEXT NOT NULL DEFAULT 'USER'
    CHECK (management_mode IN ('USER', 'PLATFORM'));

CREATE INDEX token_accounts_platform_owner_idx
    ON token_accounts (owner_type, owner_id)
    WHERE management_mode = 'PLATFORM';
