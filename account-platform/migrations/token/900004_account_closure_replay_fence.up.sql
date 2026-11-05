-- A user may have no usage rows at closure. Keep an independent permanent
-- marker, owned by schema authority rather than the runtime payload writer.
SET LOCAL ROLE token_schema_owner;
CREATE TABLE usage_replay_anonymized_users (
    user_id UUID PRIMARY KEY,
    cleared_at TIMESTAMPTZ NOT NULL
);
GRANT SELECT, INSERT ON usage_replay_anonymized_users TO token_gateway;
SET LOCAL ROLE token_gateway;
