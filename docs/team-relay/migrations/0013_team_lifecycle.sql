-- Add irreversible logical team dissolution while preserving memberships,
-- work threads and immutable action history for the retention lifecycle.

BEGIN;

ALTER TABLE teams
    ADD COLUMN dissolved_at timestamptz,
    ADD COLUMN dissolved_by uuid REFERENCES principals(id) ON DELETE RESTRICT;

ALTER TABLE teams ADD CONSTRAINT teams_dissolution_check
    CHECK (
        (dissolved_at IS NULL AND dissolved_by IS NULL)
        OR
        (dissolved_at IS NOT NULL AND dissolved_by IS NOT NULL AND dissolved_at >= created_at)
    );

CREATE UNIQUE INDEX memberships_single_active_owner_idx
    ON memberships (team_id)
    WHERE role = 'owner' AND removed_at IS NULL;

COMMENT ON INDEX memberships_single_active_owner_idx IS
    'An active team has at most one current owner; ownership is transferred transactionally.';

COMMENT ON COLUMN teams.dissolved_at IS
    'Irreversible user-visible team dissolution; historical rows remain for retention.';
COMMENT ON COLUMN teams.dissolved_by IS
    'Principal that held the active owner membership when the team was dissolved.';

INSERT INTO team_relay_schema_migrations (version) VALUES ('0013_team_lifecycle');

COMMIT;
