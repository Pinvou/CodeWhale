CREATE TABLE event_replay_requests (
    replay_id UUID PRIMARY KEY,
    event_id UUID NOT NULL REFERENCES outbox_events(event_id),
    event_digest TEXT NOT NULL CHECK (event_digest ~ '^[0-9a-f]{64}$'),
    idempotency_key TEXT NOT NULL UNIQUE CHECK (char_length(idempotency_key) BETWEEN 16 AND 128),
    request_hash TEXT NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    actor_type TEXT NOT NULL CHECK (actor_type = 'SERVICE'),
    service_name TEXT NOT NULL CHECK (service_name <> ''),
    job_type TEXT NOT NULL CHECK (job_type = 'EVENT_REPLAY'),
    job_run_id TEXT NOT NULL CHECK (job_run_id <> ''),
    reason TEXT NOT NULL CHECK (char_length(reason) >= 20),
    replayed_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX event_replay_requests_event_idx ON event_replay_requests(event_id, replayed_at DESC);

CREATE FUNCTION reject_event_replay_audit_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'event replay audit records are immutable';
END $$;

CREATE TRIGGER event_replay_requests_immutable BEFORE UPDATE OR DELETE ON event_replay_requests
FOR EACH ROW EXECUTE FUNCTION reject_event_replay_audit_mutation();
