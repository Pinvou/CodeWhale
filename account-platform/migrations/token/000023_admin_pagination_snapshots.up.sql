CREATE TABLE IF NOT EXISTS token_admin_pagination_control (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    accepting_new_snapshots BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp()
);
INSERT INTO token_admin_pagination_control(singleton,accepting_new_snapshots)
VALUES(TRUE,FALSE)
ON CONFLICT(singleton) DO NOTHING;

CREATE TABLE IF NOT EXISTS token_admin_pagination_snapshots (
    id UUID PRIMARY KEY,
    subject_id UUID NOT NULL,
    authorization_version BIGINT NOT NULL CHECK (authorization_version >= 0),
    resource_type TEXT NOT NULL CHECK (length(resource_type) BETWEEN 1 AND 80),
    scope_digest TEXT NOT NULL CHECK (scope_digest ~ '^[0-9a-f]{64}$'),
    filter_digest TEXT NOT NULL CHECK (filter_digest ~ '^[0-9a-f]{64}$'),
    sort_spec TEXT NOT NULL CHECK (length(sort_spec) BETWEEN 1 AND 512),
    page_limit INTEGER NOT NULL CHECK (page_limit BETWEEN 1 AND 200),
    high_water TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    member_count INTEGER NOT NULL CHECK (member_count BETWEEN 0 AND 1000000),
    member_bytes BIGINT NOT NULL CHECK (member_bytes >= 0),
    created_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > created_at)
);

CREATE UNIQUE INDEX IF NOT EXISTS token_admin_pagination_subject_resource_idx
    ON token_admin_pagination_snapshots(subject_id, resource_type);
CREATE INDEX IF NOT EXISTS token_admin_pagination_expiry_idx
    ON token_admin_pagination_snapshots(expires_at, id);
CREATE INDEX IF NOT EXISTS token_admin_pagination_capacity_idx
    ON token_admin_pagination_snapshots(created_at, id)
    INCLUDE (member_count, member_bytes, expires_at);

CREATE TABLE IF NOT EXISTS token_admin_pagination_snapshot_members (
    snapshot_id UUID NOT NULL REFERENCES token_admin_pagination_snapshots(id) ON DELETE CASCADE,
    -- Position 1,000,000 is an overflow sentinel; its transaction is rolled back.
    position INTEGER NOT NULL CHECK (position BETWEEN 0 AND 1000000),
    resource_id TEXT NOT NULL CHECK (length(resource_id) BETWEEN 1 AND 512),
    PRIMARY KEY (snapshot_id, position),
    UNIQUE (snapshot_id, resource_id)
);

CREATE TABLE IF NOT EXISTS token_admin_pagination_snapshot_tombstones (
    snapshot_id UUID PRIMARY KEY,
    purge_after TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS token_admin_pagination_tombstone_purge_idx
    ON token_admin_pagination_snapshot_tombstones(purge_after, snapshot_id);

CREATE TABLE IF NOT EXISTS token_admin_pagination_creation_windows (
    rate_key TEXT NOT NULL,
    window_start TIMESTAMPTZ NOT NULL,
    creation_count INTEGER NOT NULL CHECK (creation_count >= 0),
    PRIMARY KEY (rate_key, window_start)
);
CREATE INDEX IF NOT EXISTS token_admin_pagination_creation_window_expiry_idx
    ON token_admin_pagination_creation_windows(window_start);
