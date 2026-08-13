-- Bound processed avatars and store content-addressed display variants. The
-- 48/96 px variants allow continuously animated member avatars without
-- distributing the larger profile image to every list item.

BEGIN;

-- 0008 stored upload bytes before image decoding became a mandatory security
-- boundary. Those rows cannot be proven sanitized and have no display variants,
-- so fail closed during this Draft upgrade. Files become unreferenced and are
-- removed by the scheduled avatar orphan cleanup after its normal grace period.
-- Users can re-upload once to create the processed main image and both variants.
DELETE FROM principal_avatars;

ALTER TABLE principal_avatars
    DROP CONSTRAINT IF EXISTS principal_avatars_byte_size_check;

ALTER TABLE principal_avatars
    ADD CONSTRAINT principal_avatars_byte_size_check
    CHECK (byte_size > 0 AND byte_size <= 1572864);

CREATE TABLE principal_avatar_variants (
    principal_id uuid NOT NULL REFERENCES principal_avatars(principal_id) ON DELETE CASCADE,
    size smallint NOT NULL CHECK (size IN (48, 96)),
    storage_key varchar(64) NOT NULL UNIQUE CHECK (
        storage_key ~ '^[0-9a-f]{2}/[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'
    ),
    content_type varchar(32) NOT NULL CHECK (
        content_type IN ('image/png', 'image/jpeg', 'image/webp', 'image/gif')
    ),
    byte_size integer NOT NULL CHECK (byte_size > 0 AND byte_size <= 1572864),
    sha256 char(64) NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (principal_id, size)
);

COMMENT ON TABLE principal_avatar_variants IS
    'Sanitized 48/96 px avatar variants for bounded, continuously animated UI display.';

INSERT INTO team_relay_schema_migrations (version) VALUES ('0009_avatar_hardening');

COMMIT;
