DROP TABLE inbox_aggregate_versions;
ALTER TABLE inbox_messages DROP COLUMN last_error, DROP COLUMN aggregate_id;
DROP INDEX outbox_events_pending_idx;
ALTER TABLE outbox_events DROP COLUMN dead_lettered_at, DROP COLUMN locked_until, DROP COLUMN locked_by, DROP COLUMN next_attempt_at, DROP COLUMN publication_generation;
CREATE INDEX outbox_events_pending_idx ON outbox_events(occurred_at) WHERE published_at IS NULL;
