DELETE FROM role_permissions WHERE permission_code='organization.grant.reclaim_closed';
DELETE FROM permissions WHERE code='organization.grant.reclaim_closed';

DROP TABLE account_subject_retention_records;

CREATE OR REPLACE FUNCTION protect_invitation_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'invitations are retained permanently';
    END IF;
    IF OLD.id IS DISTINCT FROM NEW.id OR OLD.organization_id IS DISTINCT FROM NEW.organization_id
       OR OLD.invited_by IS DISTINCT FROM NEW.invited_by OR OLD.target_type IS DISTINCT FROM NEW.target_type
       OR OLD.target_digest IS DISTINCT FROM NEW.target_digest OR OLD.token_hash IS DISTINCT FROM NEW.token_hash
       OR OLD.idempotency_key IS DISTINCT FROM NEW.idempotency_key OR OLD.request_hash IS DISTINCT FROM NEW.request_hash THEN
        RAISE EXCEPTION 'invitation identity is immutable';
    END IF;
    RETURN NEW;
END;
$$;

UPDATE invitations SET status='REVOKED',revoked_at=COALESCE(revoked_at,updated_at)
WHERE status='CANCELLED_ACCOUNT_CLOSURE';
ALTER TABLE invitations DROP CONSTRAINT invitations_status_check;
ALTER TABLE invitations ADD CONSTRAINT invitations_status_check
    CHECK (status IN ('PENDING','ACCEPTED','REVOKED','EXPIRED'));

DROP INDEX account_closure_claim_idx;
CREATE INDEX account_closure_claim_idx ON account_closure_requests(next_attempt_at,id)
    WHERE status IN ('IN_PROGRESS','RETRY','BLOCKED')
      AND stage IN ('CLOSURE_REQUESTED','QUIESCING','FINANCIAL_CLEARANCE');

ALTER TABLE account_closure_requests
    DROP COLUMN subject_lifecycle_version,
    DROP COLUMN grants_isolated_at,
    DROP COLUMN identity_deleted_at,
    DROP COLUMN cleanup_prepared_at;
ALTER TABLE users DROP COLUMN anonymized_at, DROP COLUMN subject_lifecycle_version;
