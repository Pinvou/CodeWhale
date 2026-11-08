DO $$
BEGIN
  IF session_user<>'account_migrator' OR current_user<>'account_service' THEN
    RAISE EXCEPTION '000061 requires the isolated account migration identities' USING ERRCODE='42501';
  END IF;
  IF NOT EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=60) THEN
    RAISE EXCEPTION '000061 requires approval exclusion reason binding' USING ERRCODE='55000';
  END IF;
END $$;

CREATE TABLE approval_pagination_control (
  singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK(singleton),
  accepting_new_snapshots BOOLEAN NOT NULL DEFAULT FALSE,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp()
);
INSERT INTO approval_pagination_control(singleton,accepting_new_snapshots) VALUES(TRUE,FALSE);

CREATE TABLE approval_pagination_snapshots (
  id UUID PRIMARY KEY,
  subject_id UUID NOT NULL REFERENCES users(id),
  authorization_version BIGINT NOT NULL CHECK(authorization_version>0),
  resource_type TEXT NOT NULL CHECK(resource_type='approval_requests'),
  scope_digest TEXT NOT NULL CHECK(scope_digest~'^[0-9a-f]{64}$'),
  filter_digest TEXT NOT NULL CHECK(filter_digest~'^[0-9a-f]{64}$'),
  sort_spec TEXT NOT NULL CHECK(sort_spec='updated_at_desc_id_desc'),
  page_limit INTEGER NOT NULL CHECK(page_limit BETWEEN 1 AND 200),
  as_of TIMESTAMPTZ NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL,
  member_count INTEGER NOT NULL CHECK(member_count BETWEEN 0 AND 1000000),
  member_bytes BIGINT NOT NULL CHECK(member_bytes>=0),
  created_at TIMESTAMPTZ NOT NULL,
  CHECK(expires_at>created_at)
);
CREATE UNIQUE INDEX approval_pagination_subject_resource_idx ON approval_pagination_snapshots(subject_id,resource_type);
CREATE INDEX approval_pagination_expiry_idx ON approval_pagination_snapshots(expires_at,id);
CREATE INDEX approval_pagination_capacity_idx ON approval_pagination_snapshots(created_at,id) INCLUDE(member_count,member_bytes,expires_at);

CREATE TABLE approval_pagination_snapshot_members (
  snapshot_id UUID NOT NULL REFERENCES approval_pagination_snapshots(id) ON DELETE CASCADE,
  position INTEGER NOT NULL CHECK(position BETWEEN 0 AND 1000000),
  approval_request_id UUID NOT NULL,
  position_key TEXT NOT NULL CHECK(length(position_key) BETWEEN 38 AND 128),
  PRIMARY KEY(snapshot_id,position),
  UNIQUE(snapshot_id,approval_request_id),
  UNIQUE(snapshot_id,position_key)
);

CREATE TABLE approval_pagination_snapshot_tombstones (
  snapshot_id UUID PRIMARY KEY,
  purge_after TIMESTAMPTZ NOT NULL
);
CREATE INDEX approval_pagination_tombstone_purge_idx ON approval_pagination_snapshot_tombstones(purge_after,snapshot_id);

CREATE TABLE approval_pagination_rate_windows (
  rate_key TEXT NOT NULL CHECK(length(rate_key) BETWEEN 1 AND 512),
  window_start TIMESTAMPTZ NOT NULL,
  request_count INTEGER NOT NULL CHECK(request_count>0),
  PRIMARY KEY(rate_key,window_start)
);
CREATE INDEX approval_pagination_rate_window_expiry_idx ON approval_pagination_rate_windows(window_start);

CREATE TABLE approval_pagination_signing_keys (
  kid TEXT PRIMARY KEY CHECK(length(kid) BETWEEN 1 AND 128),
  last_signed_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX approval_pagination_signing_key_retention_idx ON approval_pagination_signing_keys(last_signed_at,kid);

REVOKE ALL ON approval_pagination_control,approval_pagination_snapshots,approval_pagination_snapshot_members,
  approval_pagination_snapshot_tombstones,approval_pagination_rate_windows,approval_pagination_signing_keys FROM PUBLIC;
