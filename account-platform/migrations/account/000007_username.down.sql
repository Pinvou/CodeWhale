DROP INDEX users_username_unique_idx;
ALTER TABLE users
    DROP CONSTRAINT users_username_length_check,
    DROP COLUMN username;
