ALTER TABLE member_quota_grant_audits DROP CONSTRAINT member_quota_grant_audits_action_check;
ALTER TABLE member_quota_grant_audits ADD CONSTRAINT member_quota_grant_audits_action_check
    CHECK (action IN ('ALLOCATED','SUSPENDED','RESTORED','REVOKED','HOLD_RESERVED','HOLD_RELEASED','HOLD_SETTLED','EXPIRED','ACCOUNT_CLOSED_ISOLATED','RECLAIMED_ACCOUNT_CLOSED'));

CREATE TABLE account_closure_grant_isolations (
    closure_request_id UUID PRIMARY KEY,
    member_user_id UUID NOT NULL,
    subject_lifecycle_version BIGINT NOT NULL CHECK (subject_lifecycle_version>0),
    isolated_grant_count INTEGER NOT NULL CHECK (isolated_grant_count>=0),
    response_snapshot JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);
CREATE UNIQUE INDEX account_closure_grant_isolations_subject_version_idx
    ON account_closure_grant_isolations(member_user_id,subject_lifecycle_version);

CREATE TABLE account_closed_grant_reclaim_commands (
    organization_id UUID NOT NULL,
    idempotency_key TEXT NOT NULL CHECK (char_length(idempotency_key) BETWEEN 16 AND 128),
    request_fingerprint TEXT NOT NULL CHECK (request_fingerprint ~ '^[0-9a-f]{64}$'),
    grant_id UUID NOT NULL REFERENCES member_quota_grants(id),
    actor_user_id UUID NOT NULL,
    reason TEXT NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 500),
    response_snapshot JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (organization_id,idempotency_key)
);

CREATE FUNCTION protect_account_closed_grant_commands() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'account closure grant evidence is immutable';
END $$;
CREATE TRIGGER account_closure_grant_isolations_immutable
BEFORE UPDATE OR DELETE ON account_closure_grant_isolations
FOR EACH ROW EXECUTE FUNCTION protect_account_closed_grant_commands();
CREATE TRIGGER account_closed_grant_reclaim_commands_immutable
BEFORE UPDATE OR DELETE ON account_closed_grant_reclaim_commands
FOR EACH ROW EXECUTE FUNCTION protect_account_closed_grant_commands();
