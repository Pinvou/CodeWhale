-- Add a reusable, owner-rotated team join password. Only its SHA-256 digest is
-- stored; the generated high-entropy plaintext is returned once to the owner.

BEGIN;

ALTER TABLE teams
    ADD COLUMN join_password_hash char(64)
        CHECK (join_password_hash IS NULL OR join_password_hash ~ '^[0-9a-f]{64}$'),
    ADD COLUMN join_password_updated_at timestamptz,
    ADD COLUMN join_password_updated_by uuid REFERENCES principals(id) ON DELETE RESTRICT,
    ADD CONSTRAINT teams_join_password_metadata_consistent CHECK (
        (join_password_hash IS NULL AND join_password_updated_at IS NULL AND join_password_updated_by IS NULL)
        OR
        (join_password_hash IS NOT NULL AND join_password_updated_at IS NOT NULL AND join_password_updated_by IS NOT NULL)
    );

CREATE UNIQUE INDEX teams_join_password_hash_unique_idx
    ON teams (join_password_hash)
    WHERE join_password_hash IS NOT NULL;

COMMENT ON COLUMN teams.join_password_hash IS
    'SHA-256 digest of the reusable high-entropy join password; rotating it invalidates the previous password.';

INSERT INTO team_relay_schema_migrations (version) VALUES ('0004_fixed_join_password');

COMMIT;
