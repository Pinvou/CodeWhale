ALTER TABLE users
    ADD COLUMN subject_lifecycle_version BIGINT NOT NULL DEFAULT 1 CHECK (subject_lifecycle_version>0),
    ADD COLUMN anonymized_at TIMESTAMPTZ;

ALTER TABLE account_closure_requests
    ADD COLUMN cleanup_prepared_at TIMESTAMPTZ,
    ADD COLUMN identity_deleted_at TIMESTAMPTZ,
    ADD COLUMN grants_isolated_at TIMESTAMPTZ,
    ADD COLUMN subject_lifecycle_version BIGINT CHECK (subject_lifecycle_version>0);

DROP INDEX account_closure_claim_idx;
CREATE INDEX account_closure_claim_idx ON account_closure_requests(next_attempt_at,id)
    WHERE status IN ('IN_PROGRESS','RETRY','BLOCKED')
      AND stage IN ('CLOSURE_REQUESTED','QUIESCING','FINANCIAL_CLEARANCE','IDENTITY_CLEANUP','ANONYMIZED');

ALTER TABLE invitations DROP CONSTRAINT invitations_status_check;
ALTER TABLE invitations ADD CONSTRAINT invitations_status_check
    CHECK (status IN ('PENDING','ACCEPTED','REVOKED','EXPIRED','CANCELLED_ACCOUNT_CLOSURE'));

CREATE OR REPLACE FUNCTION protect_invitation_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'invitations are retained permanently';
    END IF;
    IF OLD.id IS DISTINCT FROM NEW.id OR OLD.organization_id IS DISTINCT FROM NEW.organization_id
       OR OLD.invited_by IS DISTINCT FROM NEW.invited_by OR OLD.target_type IS DISTINCT FROM NEW.target_type
       OR OLD.idempotency_key IS DISTINCT FROM NEW.idempotency_key OR OLD.request_hash IS DISTINCT FROM NEW.request_hash THEN
        RAISE EXCEPTION 'invitation identity is immutable';
    END IF;
    IF (OLD.target_digest IS DISTINCT FROM NEW.target_digest OR OLD.token_hash IS DISTINCT FROM NEW.token_hash)
       AND NOT (OLD.status='PENDING' AND NEW.status='CANCELLED_ACCOUNT_CLOSURE') THEN
        RAISE EXCEPTION 'invitation target can only be erased during account closure';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TABLE account_subject_retention_records (
    user_id UUID PRIMARY KEY REFERENCES users(id),
    user_code TEXT NOT NULL,
    retention_class TEXT NOT NULL CHECK (retention_class='ACCOUNT_CLOSURE_LEGAL_RECORD'),
    retained_until TIMESTAMPTZ NOT NULL,
    legal_hold BOOLEAN NOT NULL DEFAULT FALSE,
    status TEXT NOT NULL CHECK (status IN ('RETAINED','PURGED')),
    created_at TIMESTAMPTZ NOT NULL,
    purged_at TIMESTAMPTZ,
    CHECK ((status='PURGED')=(purged_at IS NOT NULL))
);

INSERT INTO permissions(code,scope,description) VALUES
('organization.grant.reclaim_closed','ORGANIZATION','Reclaim an eligible grant isolated by irreversible account closure.');
INSERT INTO role_permissions(role_code,permission_code) VALUES
('OWNER','organization.grant.reclaim_closed'),
('ORG_ADMIN','organization.grant.reclaim_closed'),
('BILLING_ADMIN','organization.grant.reclaim_closed');
