ALTER TABLE users
    ADD COLUMN username TEXT,
    ADD CONSTRAINT users_username_length_check
        CHECK (username IS NULL OR char_length(username) BETWEEN 1 AND 255);

CREATE UNIQUE INDEX users_username_unique_idx
    ON users (lower(username))
    WHERE username IS NOT NULL AND status <> 'DELETED';
