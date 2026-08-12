-- Add personal work-list preferences and an auditable cancellation state.
-- Preferences are scoped to a principal and never mutate the shared thread log.

BEGIN;

ALTER TABLE work_threads DROP CONSTRAINT IF EXISTS work_threads_phase_check;
ALTER TABLE work_threads ADD CONSTRAINT work_threads_phase_check
    CHECK (phase IN ('active', 'awaiting_acceptance', 'closed', 'cancelled'));

ALTER TABLE work_threads DROP CONSTRAINT IF EXISTS work_threads_check;
ALTER TABLE work_threads ADD CONSTRAINT work_threads_state_check
    CHECK (
        (phase = 'active' AND current_actor_id IS NOT NULL AND return_to_id IS NOT NULL)
        OR (phase = 'awaiting_acceptance' AND current_actor_id = final_recipient_id AND return_to_id IS NULL)
        OR (phase IN ('closed', 'cancelled') AND current_actor_id IS NULL AND return_to_id IS NULL)
    );

DROP INDEX IF EXISTS work_threads_current_actor_idx;
CREATE INDEX work_threads_current_actor_idx
    ON work_threads (team_id, current_actor_id, updated_at DESC)
    WHERE phase NOT IN ('closed', 'cancelled');

ALTER TABLE relay_actions DROP CONSTRAINT IF EXISTS relay_actions_kind_check;
ALTER TABLE relay_actions ADD CONSTRAINT relay_actions_kind_check
    CHECK (kind IN ('assign', 'note', 'handoff', 'return', 'final_deliver', 'request_changes', 'accept', 'cancel'));

ALTER TABLE relay_actions DROP CONSTRAINT IF EXISTS relay_actions_check;
ALTER TABLE relay_actions ADD CONSTRAINT relay_actions_shape_check
    CHECK (
        (kind = 'assign' AND recipient_id IS NOT NULL AND return_to_id IS NOT NULL)
        OR (kind = 'handoff' AND recipient_id IS NOT NULL AND return_to_id IS NOT NULL)
        OR (kind = 'return' AND recipient_id IS NOT NULL AND return_to_id IS NOT NULL)
        OR (kind = 'final_deliver' AND recipient_id IS NOT NULL AND return_to_id IS NULL)
        OR (kind = 'request_changes' AND recipient_id IS NOT NULL AND return_to_id IS NOT NULL)
        OR (kind IN ('note', 'accept', 'cancel') AND recipient_id IS NULL AND return_to_id IS NULL)
    );

CREATE TABLE thread_preferences (
    principal_id uuid NOT NULL REFERENCES principals(id) ON DELETE RESTRICT,
    thread_id uuid NOT NULL REFERENCES work_threads(id) ON DELETE RESTRICT,
    pinned_at timestamptz,
    hidden_at timestamptz,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (principal_id, thread_id)
);

CREATE INDEX thread_preferences_principal_pinned_idx
    ON thread_preferences (principal_id, pinned_at DESC) WHERE pinned_at IS NOT NULL;
CREATE INDEX thread_preferences_principal_hidden_idx
    ON thread_preferences (principal_id, hidden_at DESC) WHERE hidden_at IS NOT NULL;

COMMENT ON TABLE thread_preferences IS
    'Per-principal work-list preferences. Pinning and hiding sync across that principal devices and do not affect teammates.';

INSERT INTO team_relay_schema_migrations (version) VALUES ('0003_thread_management');

COMMIT;
