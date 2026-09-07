-- Persist per-principal read cursors so unread relay updates stay consistent
-- across devices. Existing participants start at the current thread revision to
-- avoid turning the complete historical log into unread work after deployment.

BEGIN;

CREATE TABLE thread_read_states (
    principal_id uuid NOT NULL REFERENCES principals(id) ON DELETE RESTRICT,
    thread_id uuid NOT NULL REFERENCES work_threads(id) ON DELETE RESTRICT,
    last_read_sequence bigint NOT NULL CHECK (last_read_sequence >= 0),
    last_read_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (principal_id, thread_id)
);

INSERT INTO thread_read_states (principal_id, thread_id, last_read_sequence, last_read_at)
SELECT tp.principal_id, tp.thread_id, w.revision, now()
  FROM thread_participants tp
  JOIN work_threads w ON w.id = tp.thread_id;

CREATE INDEX thread_read_states_principal_updated_idx
    ON thread_read_states (principal_id, last_read_at DESC);

COMMENT ON TABLE thread_read_states IS
    'Per-principal monotonic relay-action read cursors shared by all devices for that principal.';

INSERT INTO team_relay_schema_migrations (version) VALUES ('0008_thread_read_states');

COMMIT;
