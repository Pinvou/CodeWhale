CREATE TABLE outbox_events (
    event_id UUID PRIMARY KEY,
    event_type TEXT NOT NULL,
    aggregate_id UUID NOT NULL,
    aggregate_version BIGINT NOT NULL CHECK (aggregate_version > 0),
    payload JSONB NOT NULL,
    trace_id TEXT NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    published_at TIMESTAMPTZ,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error TEXT
);

CREATE INDEX outbox_events_pending_idx
    ON outbox_events (occurred_at)
    WHERE published_at IS NULL;

CREATE TABLE inbox_messages (
    event_id UUID PRIMARY KEY,
    event_type TEXT NOT NULL,
    aggregate_version BIGINT NOT NULL CHECK (aggregate_version > 0),
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at TIMESTAMPTZ
);

CREATE TABLE audit_logs (
    id UUID PRIMARY KEY,
    actor_user_id UUID,
    session_id TEXT,
    active_organization_id UUID,
    action TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    resource_id TEXT NOT NULL,
    result TEXT NOT NULL CHECK (result IN ('SUCCEEDED', 'DENIED', 'FAILED')),
    request_id TEXT NOT NULL,
    trace_id TEXT NOT NULL,
    change_summary JSONB NOT NULL DEFAULT '{}'::jsonb,
    occurred_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX audit_logs_resource_idx
    ON audit_logs (resource_type, resource_id, occurred_at DESC);
