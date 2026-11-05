CREATE TABLE usage_hold_lease_events (
    id UUID PRIMARY KEY,
    hold_id UUID NOT NULL REFERENCES usage_holds(id),
    event_type TEXT NOT NULL CHECK (event_type IN ('CREATED','EXTENDED')),
    previous_expires_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ NOT NULL,
    previous_max_microcredits BIGINT NOT NULL CHECK (previous_max_microcredits >= 0),
    max_microcredits BIGINT NOT NULL CHECK (max_microcredits >= 0),
    additional_microcredits BIGINT NOT NULL CHECK (additional_microcredits >= 0),
    created_at TIMESTAMPTZ NOT NULL
);

INSERT INTO usage_hold_lease_events(
    id,hold_id,event_type,previous_expires_at,expires_at,
    previous_max_microcredits,max_microcredits,additional_microcredits,created_at
)
SELECT id,id,'CREATED',NULL,expires_at,0,max_microcredits,max_microcredits,created_at
FROM usage_holds;

CREATE INDEX usage_hold_lease_events_hold_idx
    ON usage_hold_lease_events(hold_id,created_at,id);

CREATE TABLE usage_hold_admin_actions (
    id UUID PRIMARY KEY,
    hold_id UUID NOT NULL REFERENCES usage_holds(id),
    action TEXT NOT NULL CHECK (action IN ('SAFE_RELEASE')),
    idempotency_key TEXT NOT NULL,
    request_fingerprint TEXT NOT NULL,
    reason TEXT NOT NULL,
    actor TEXT NOT NULL,
    before_status TEXT NOT NULL,
    after_status TEXT NOT NULL,
    allocation_snapshot JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE(action,idempotency_key)
);

CREATE INDEX usage_hold_admin_actions_hold_idx
    ON usage_hold_admin_actions(hold_id,created_at,id);

CREATE OR REPLACE FUNCTION reject_usage_hold_evidence_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'usage hold operational evidence is immutable';
END $$;

CREATE TRIGGER usage_hold_lease_events_immutable
BEFORE UPDATE OR DELETE ON usage_hold_lease_events
FOR EACH ROW EXECUTE FUNCTION reject_usage_hold_evidence_mutation();

CREATE TRIGGER usage_hold_admin_actions_immutable
BEFORE UPDATE OR DELETE ON usage_hold_admin_actions
FOR EACH ROW EXECUTE FUNCTION reject_usage_hold_evidence_mutation();

-- Bind a logical operator decision before contacting the Token authority.
CREATE TABLE usage_hold_admin_commands (
    idempotency_key TEXT PRIMARY KEY,
    hold_id UUID NOT NULL REFERENCES usage_holds(id),
    request_fingerprint TEXT NOT NULL,
    reason TEXT NOT NULL,
    actor TEXT NOT NULL,
    before_status TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);
CREATE TRIGGER usage_hold_admin_commands_immutable
BEFORE UPDATE OR DELETE ON usage_hold_admin_commands
FOR EACH ROW EXECUTE FUNCTION reject_usage_hold_evidence_mutation();
