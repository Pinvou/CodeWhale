-- Store one independently managed avatar per Principal. Avatar bytes live in
-- the existing private attachment volume; this table only keeps verified
-- metadata and an opaque, server-generated storage key.

BEGIN;

CREATE TABLE principal_avatars (
    principal_id uuid PRIMARY KEY REFERENCES principals(id) ON DELETE CASCADE,
    storage_key varchar(64) NOT NULL UNIQUE CHECK (
        storage_key ~ '^[0-9a-f]{2}/[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'
    ),
    content_type varchar(32) NOT NULL CHECK (
        content_type IN ('image/png', 'image/jpeg', 'image/webp', 'image/gif')
    ),
    byte_size integer NOT NULL CHECK (byte_size > 0 AND byte_size <= 5242880),
    sha256 char(64) NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE principal_avatars IS
    'Verified custom Team Relay avatars. Bytes are private and require shared-team authorization.';

INSERT INTO team_relay_schema_migrations (version) VALUES ('0008_principal_avatars');

COMMIT;
