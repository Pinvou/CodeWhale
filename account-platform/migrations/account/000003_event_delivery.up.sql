DROP INDEX outbox_events_pending_idx;
ALTER TABLE outbox_events
    ADD COLUMN publication_generation BIGINT NOT NULL DEFAULT 0 CHECK (publication_generation >= 0),
    ADD COLUMN next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD COLUMN locked_by TEXT,
    ADD COLUMN locked_until TIMESTAMPTZ,
    ADD COLUMN dead_lettered_at TIMESTAMPTZ;
CREATE INDEX outbox_events_pending_idx ON outbox_events(next_attempt_at, occurred_at)
    WHERE published_at IS NULL AND dead_lettered_at IS NULL;

ALTER TABLE inbox_messages
    ADD COLUMN aggregate_id UUID,
    ADD COLUMN last_error TEXT;
CREATE TABLE inbox_aggregate_versions (
    event_type TEXT NOT NULL,
    aggregate_id UUID NOT NULL,
    last_version BIGINT NOT NULL CHECK (last_version > 0),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (event_type, aggregate_id)
);
