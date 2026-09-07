-- Store a static first-frame variant beside each animated GIF display variant.
-- This lets clients honor prefers-reduced-motion without downloading or
-- decoding the animated payload.

BEGIN;

ALTER TABLE principal_avatar_variants
    ADD COLUMN animation_mode varchar(16) NOT NULL DEFAULT 'animated'
    CHECK (animation_mode IN ('animated', 'static'));

ALTER TABLE principal_avatar_variants
    DROP CONSTRAINT principal_avatar_variants_pkey;

ALTER TABLE principal_avatar_variants
    ADD PRIMARY KEY (principal_id, size, animation_mode);

COMMENT ON TABLE principal_avatar_variants IS
    'Sanitized 48/96 px avatar variants, including static GIF first frames for reduced motion.';

INSERT INTO team_relay_schema_migrations (version) VALUES ('0012_avatar_reduced_motion');

COMMIT;
