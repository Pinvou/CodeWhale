CREATE EXTENSION IF NOT EXISTS pgcrypto;

ALTER TABLE users ADD COLUMN user_code TEXT;

CREATE FUNCTION user_code_checksum(base_code TEXT) RETURNS TEXT
LANGUAGE plpgsql IMMUTABLE STRICT AS $$
DECLARE
    alphabet CONSTANT TEXT := '0123456789ABCDEFGHJKMNPQRSTVWXYZ';
    hash BYTEA;
    first_ten_bits INTEGER;
BEGIN
    hash := digest(convert_to(base_code, 'UTF8'), 'sha256');
    first_ten_bits := (get_byte(hash, 0) << 2) | (get_byte(hash, 1) >> 6);
    RETURN substr(alphabet, (first_ten_bits >> 5) + 1, 1)
        || substr(alphabet, (first_ten_bits & 31) + 1, 1);
END;
$$;

CREATE FUNCTION valid_user_code(candidate TEXT) RETURNS BOOLEAN
LANGUAGE sql IMMUTABLE STRICT AS $$
    SELECT candidate ~ '^USR[0-9A-HJKMNP-TV-Z]{18}$'
       AND right(candidate, 2) = user_code_checksum(left(candidate, 19));
$$;

CREATE FUNCTION generate_user_code_candidate() RETURNS TEXT
LANGUAGE plpgsql VOLATILE AS $$
DECLARE
    alphabet CONSTANT TEXT := '0123456789ABCDEFGHJKMNPQRSTVWXYZ';
    random_bytes BYTEA := gen_random_bytes(16);
    payload TEXT := '';
    base_code TEXT;
    index INTEGER;
BEGIN
    FOR index IN 0..15 LOOP
        payload := payload || substr(alphabet, (get_byte(random_bytes, index) & 31) + 1, 1);
    END LOOP;
    base_code := 'USR' || payload;
    RETURN base_code || user_code_checksum(base_code);
END;
$$;

CREATE FUNCTION generate_unique_user_code() RETURNS TEXT
LANGUAGE plpgsql VOLATILE AS $$
DECLARE
    candidate TEXT;
BEGIN
    LOOP
        candidate := generate_user_code_candidate();
        EXIT WHEN NOT EXISTS (SELECT 1 FROM users WHERE user_code = candidate);
    END LOOP;
    RETURN candidate;
END;
$$;

DO $$
DECLARE
    existing_user RECORD;
BEGIN
    FOR existing_user IN SELECT id FROM users WHERE user_code IS NULL ORDER BY id LOOP
        UPDATE users SET user_code = generate_unique_user_code() WHERE id = existing_user.id;
    END LOOP;
END;
$$;

ALTER TABLE users
    ALTER COLUMN user_code SET NOT NULL,
    ADD CONSTRAINT users_user_code_unique UNIQUE (user_code),
    ADD CONSTRAINT users_user_code_format_check CHECK (valid_user_code(user_code));

CREATE TABLE user_code_registry (
    user_code TEXT PRIMARY KEY CHECK (valid_user_code(user_code)),
    user_id UUID NOT NULL UNIQUE,
    registered_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE user_code_lookup_attempts (
    id UUID PRIMARY KEY,
    actor_user_id UUID NOT NULL REFERENCES users(id),
    source_hash BYTEA NOT NULL,
    device_hash BYTEA NOT NULL,
    target_code_hash BYTEA NOT NULL,
    result TEXT NOT NULL CHECK (result IN ('SUCCEEDED', 'NOT_FOUND', 'DENIED', 'RATE_LIMITED')),
    occurred_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX user_code_lookup_attempts_actor_idx ON user_code_lookup_attempts(actor_user_id, occurred_at DESC);
CREATE INDEX user_code_lookup_attempts_source_idx ON user_code_lookup_attempts(source_hash, occurred_at DESC);
CREATE INDEX user_code_lookup_attempts_device_idx ON user_code_lookup_attempts(device_hash, occurred_at DESC);
CREATE INDEX user_code_lookup_attempts_target_idx ON user_code_lookup_attempts(target_code_hash, occurred_at DESC);

INSERT INTO user_code_registry(user_code, user_id, registered_at)
SELECT user_code, id, created_at FROM users;

CREATE FUNCTION protect_permanent_user_identifiers() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP IN ('DELETE', 'TRUNCATE') THEN
        RAISE EXCEPTION 'physical user deletion is forbidden; anonymize the existing subject instead'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.id IS DISTINCT FROM OLD.id THEN
        RAISE EXCEPTION 'user_id is permanently immutable'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.user_code IS DISTINCT FROM OLD.user_code THEN
        RAISE EXCEPTION 'user_code is permanently immutable'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER users_permanent_identifiers
BEFORE UPDATE OR DELETE ON users
FOR EACH ROW EXECUTE FUNCTION protect_permanent_user_identifiers();

CREATE TRIGGER users_permanent_identifiers_no_truncate
BEFORE TRUNCATE ON users
FOR EACH STATEMENT EXECUTE FUNCTION protect_permanent_user_identifiers();

CREATE FUNCTION register_permanent_user_code() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO user_code_registry(user_code, user_id, registered_at)
    VALUES (NEW.user_code, NEW.id, NEW.created_at);
    RETURN NEW;
END;
$$;

CREATE TRIGGER users_register_permanent_code
AFTER INSERT ON users
FOR EACH ROW EXECUTE FUNCTION register_permanent_user_code();

CREATE FUNCTION protect_user_code_registry() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'user code registry entries are permanent and immutable'
        USING ERRCODE = 'integrity_constraint_violation';
END;
$$;

CREATE TRIGGER user_code_registry_immutable
BEFORE UPDATE OR DELETE ON user_code_registry
FOR EACH ROW EXECUTE FUNCTION protect_user_code_registry();

CREATE TRIGGER user_code_registry_no_truncate
BEFORE TRUNCATE ON user_code_registry
FOR EACH STATEMENT EXECUTE FUNCTION protect_user_code_registry();
