-- Add an optional, Hub-scoped email identity to existing principals. Existing
-- principals remain valid and can bind an email later without changing IDs or
-- losing memberships and relay history.

BEGIN;

ALTER TABLE principals
    ADD COLUMN email varchar(254),
    ADD CONSTRAINT principals_email_normalized_check CHECK (
        email IS NULL
        OR (
            email = lower(btrim(email))
            AND char_length(email) BETWEEN 3 AND 254
            AND email LIKE '%@%'
        )
    );

CREATE UNIQUE INDEX principals_email_unique_idx
    ON principals (email)
    WHERE email IS NOT NULL;

COMMENT ON COLUMN principals.email IS
    'Normalized Hub-local account email. NULL only for principals created before email registration.';

INSERT INTO team_relay_schema_migrations (version) VALUES ('0007_principal_email');

COMMIT;
