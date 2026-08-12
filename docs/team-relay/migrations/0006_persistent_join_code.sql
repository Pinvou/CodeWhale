-- Persist an encrypted copy of the reusable join code so authorized members can
-- retrieve it. The hash remains authoritative for joins; plaintext is never stored.

BEGIN;

ALTER TABLE teams
    ADD COLUMN join_code_ciphertext text,
    ADD COLUMN join_code_visibility varchar(16) NOT NULL DEFAULT 'members'
        CHECK (join_code_visibility IN ('members', 'owners'));

COMMENT ON COLUMN teams.join_code_ciphertext IS
    'AES-256-GCM encrypted display form of the team join code; NULL for legacy hash-only codes.';
COMMENT ON COLUMN teams.join_code_visibility IS
    'members allows every active member to retrieve the code; owners hides it from ordinary members.';

INSERT INTO team_relay_schema_migrations (version) VALUES ('0006_persistent_join_code');

COMMIT;
