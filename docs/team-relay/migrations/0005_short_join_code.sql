-- Keep the existing digest column while documenting the short, normalized
-- team join code introduced by the application protocol.

BEGIN;

COMMENT ON COLUMN teams.join_password_hash IS
    'SHA-256 digest of the reusable normalized team join code; rotating it invalidates the previous code.';

INSERT INTO team_relay_schema_migrations (version) VALUES ('0005_short_join_code');

COMMIT;
