-- Require recipients to accept assignments and collaboration transfers before
-- responsibility or material access moves to them. Existing active work is
-- already accepted and therefore remains unchanged.

BEGIN;

ALTER TABLE work_threads
    ADD COLUMN pending_transfer_kind varchar(16),
    ADD COLUMN pending_sender_id uuid REFERENCES principals(id) ON DELETE RESTRICT,
    ADD COLUMN pending_recipient_id uuid REFERENCES principals(id) ON DELETE RESTRICT;

ALTER TABLE devices
    ADD COLUMN relay_protocol_version varchar(8) NOT NULL DEFAULT '0.1'
        CHECK (relay_protocol_version IN ('0.1', '0.2')),
    ADD COLUMN relay_protocol_seen_at timestamptz;

ALTER TABLE work_threads DROP CONSTRAINT IF EXISTS work_threads_phase_check;
ALTER TABLE work_threads ADD CONSTRAINT work_threads_phase_check
    CHECK (phase IN (
        'pending_transfer', 'awaiting_reassignment', 'active',
        'awaiting_acceptance', 'closed', 'cancelled'
    ));

ALTER TABLE work_threads DROP CONSTRAINT IF EXISTS work_threads_state_check;
ALTER TABLE work_threads ADD CONSTRAINT work_threads_state_check
    CHECK (
        (
            phase = 'pending_transfer'
            AND current_actor_id IS NOT NULL
            AND return_to_id IS NOT NULL
            AND pending_transfer_kind IN ('assignment', 'handoff')
            AND pending_sender_id IS NOT NULL
            AND pending_recipient_id IS NOT NULL
            AND pending_sender_id <> pending_recipient_id
        )
        OR (
            phase = 'awaiting_reassignment'
            AND owner_id = initiator_id
            AND current_actor_id = initiator_id
            AND return_to_id = final_recipient_id
            AND pending_transfer_kind IS NULL
            AND pending_sender_id IS NULL
            AND pending_recipient_id IS NULL
        )
        OR (
            phase = 'active'
            AND current_actor_id IS NOT NULL
            AND return_to_id IS NOT NULL
            AND pending_transfer_kind IS NULL
            AND pending_sender_id IS NULL
            AND pending_recipient_id IS NULL
        )
        OR (
            phase = 'awaiting_acceptance'
            AND current_actor_id = final_recipient_id
            AND return_to_id IS NULL
            AND pending_transfer_kind IS NULL
            AND pending_sender_id IS NULL
            AND pending_recipient_id IS NULL
        )
        OR (
            phase IN ('closed', 'cancelled')
            AND current_actor_id IS NULL
            AND return_to_id IS NULL
            AND pending_transfer_kind IS NULL
            AND pending_sender_id IS NULL
            AND pending_recipient_id IS NULL
        )
    );

ALTER TABLE relay_actions DROP CONSTRAINT IF EXISTS relay_actions_kind_check;
ALTER TABLE relay_actions ADD COLUMN transfer_mode varchar(16);
UPDATE relay_actions
   SET transfer_mode = 'direct_legacy'
 WHERE kind IN ('assign', 'handoff');
ALTER TABLE relay_actions ADD CONSTRAINT relay_actions_kind_check
    CHECK (kind IN (
        'assign', 'note', 'handoff', 'accept_transfer', 'decline_transfer',
        'withdraw_transfer', 'reroute_transfer', 'return', 'final_deliver',
        'request_changes', 'accept', 'cancel'
    ));

ALTER TABLE relay_actions DROP CONSTRAINT IF EXISTS relay_actions_shape_check;
ALTER TABLE relay_actions ADD CONSTRAINT relay_actions_shape_check
    CHECK (
        (kind = 'assign' AND recipient_id IS NOT NULL AND return_to_id IS NOT NULL)
        OR (kind = 'handoff' AND recipient_id IS NOT NULL AND return_to_id IS NOT NULL)
        OR (kind IN ('accept_transfer', 'decline_transfer', 'withdraw_transfer', 'reroute_transfer')
            AND recipient_id IS NOT NULL AND return_to_id IS NULL)
        OR (kind = 'return' AND recipient_id IS NOT NULL AND return_to_id IS NOT NULL)
        OR (kind = 'final_deliver' AND recipient_id IS NOT NULL AND return_to_id IS NULL)
        OR (kind = 'request_changes' AND recipient_id IS NOT NULL AND return_to_id IS NOT NULL)
        OR (kind IN ('note', 'accept', 'cancel') AND recipient_id IS NULL AND return_to_id IS NULL)
    );

ALTER TABLE relay_actions ADD CONSTRAINT relay_actions_transfer_mode_check
    CHECK (
        (kind IN ('assign', 'handoff', 'reroute_transfer')
            AND transfer_mode IN ('confirmed', 'direct_legacy'))
        OR
        (kind NOT IN ('assign', 'handoff', 'reroute_transfer')
            AND transfer_mode IS NULL)
    );

ALTER TABLE thread_participants ADD COLUMN material_access_at timestamptz;
UPDATE thread_participants SET material_access_at = created_at WHERE material_access_at IS NULL;

CREATE INDEX work_threads_pending_recipient_idx
    ON work_threads (team_id, pending_recipient_id, updated_at DESC)
    WHERE phase = 'pending_transfer';

COMMENT ON COLUMN thread_participants.material_access_at IS
    'Null while a proposed recipient can only see the transfer summary; set after acceptance.';

COMMENT ON COLUMN devices.relay_protocol_version IS
    'Team Relay protocol declared by this device during its latest capability registration.';
COMMENT ON COLUMN relay_actions.transfer_mode IS
    'Authoritative mode selected by the Hub for assignment, handoff or reroute.';

INSERT INTO team_relay_schema_migrations (version) VALUES ('0009_confirmed_transfers');

COMMIT;
