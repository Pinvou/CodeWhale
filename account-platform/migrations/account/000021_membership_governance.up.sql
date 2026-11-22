ALTER TABLE invitations
    ALTER COLUMN email_normalized DROP NOT NULL,
    ADD COLUMN target_type TEXT NOT NULL DEFAULT 'EMAIL',
    ADD COLUMN target_digest BYTEA,
    ADD COLUMN target_hint TEXT,
    ADD COLUMN idempotency_key TEXT,
    ADD COLUMN request_hash TEXT,
    ADD COLUMN version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
    ADD COLUMN accepted_at TIMESTAMPTZ,
    ADD COLUMN revoked_at TIMESTAMPTZ;

UPDATE invitations
SET target_digest=digest(convert_to(lower(email_normalized),'UTF8'),'sha256'),
    target_hint=regexp_replace(email_normalized,'(^.).*(@.*$)','\1***\2')
WHERE target_digest IS NULL;

ALTER TABLE invitations
    ADD CONSTRAINT invitations_target_type_check CHECK (target_type IN ('EMAIL','PHONE','USERNAME','LINK')),
    ADD CONSTRAINT invitations_target_presence_check CHECK ((target_type='LINK')=(target_digest IS NULL)),
    ADD CONSTRAINT invitations_idempotency_key_check CHECK (idempotency_key IS NULL OR char_length(idempotency_key) BETWEEN 8 AND 120),
    ADD CONSTRAINT invitations_request_hash_check CHECK (request_hash IS NULL OR char_length(request_hash)=64);

DROP INDEX invitations_pending_email_idx;
CREATE INDEX invitations_pending_target_idx
    ON invitations (target_type,target_digest,expires_at,id)
    WHERE status='PENDING';
CREATE UNIQUE INDEX invitations_idempotency_idx
    ON invitations (invited_by,organization_id,idempotency_key)
    WHERE idempotency_key IS NOT NULL;

UPDATE role_bindings binding
SET revoked_at=GREATEST(membership.updated_at,binding.created_at)
FROM memberships membership
WHERE binding.membership_id=membership.id
  AND membership.status='REVOKED' AND binding.revoked_at IS NULL;

CREATE TABLE membership_commands (
    actor_user_id UUID NOT NULL REFERENCES users(id),
    organization_id UUID NOT NULL REFERENCES organizations(id),
    target_user_id UUID NOT NULL REFERENCES users(id),
    command_type TEXT NOT NULL CHECK (command_type IN ('ADD','REMOVE','TRANSFER_OWNERSHIP')),
    idempotency_key TEXT NOT NULL CHECK (char_length(idempotency_key) BETWEEN 8 AND 120),
    request_hash TEXT NOT NULL CHECK (char_length(request_hash)=64),
    membership_id UUID NOT NULL REFERENCES memberships(id),
    result JSONB NOT NULL CHECK (jsonb_typeof(result)='object'),
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (actor_user_id,organization_id,command_type,idempotency_key)
);

CREATE FUNCTION protect_membership_governance_history() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'membership governance history is immutable';
END;
$$;

CREATE TRIGGER membership_commands_immutable
BEFORE UPDATE OR DELETE ON membership_commands
FOR EACH ROW EXECUTE FUNCTION protect_membership_governance_history();

CREATE TRIGGER role_bindings_no_physical_delete
BEFORE DELETE ON role_bindings
FOR EACH ROW EXECUTE FUNCTION protect_membership_governance_history();

CREATE FUNCTION protect_invitation_identity() RETURNS trigger LANGUAGE plpgsql AS $$
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

CREATE TRIGGER invitations_identity_guard
BEFORE UPDATE OR DELETE ON invitations
FOR EACH ROW EXECUTE FUNCTION protect_invitation_identity();

CREATE FUNCTION enforce_single_effective_owner() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    affected_organization UUID;
    organization_status TEXT;
    owner_count BIGINT;
BEGIN
    IF TG_TABLE_NAME='organizations' THEN
        affected_organization:=CASE WHEN TG_OP='DELETE' THEN OLD.id ELSE NEW.id END;
    ELSIF TG_TABLE_NAME='memberships' THEN
        affected_organization:=CASE WHEN TG_OP='DELETE' THEN OLD.organization_id ELSE NEW.organization_id END;
    ELSE
        SELECT organization_id INTO affected_organization
        FROM memberships WHERE id=CASE WHEN TG_OP='DELETE' THEN OLD.membership_id ELSE NEW.membership_id END;
    END IF;
    SELECT status INTO organization_status FROM organizations WHERE id=affected_organization;
    IF organization_status IS NULL OR organization_status='DELETED' THEN
        RETURN NULL;
    END IF;
    SELECT count(*) INTO owner_count
    FROM memberships membership
    JOIN role_bindings binding ON binding.membership_id=membership.id
    WHERE membership.organization_id=affected_organization
      AND membership.status<>'REVOKED'
      AND binding.role='OWNER' AND binding.revoked_at IS NULL
      AND binding.starts_at<=now() AND (binding.expires_at IS NULL OR binding.expires_at>now());
    IF owner_count<>1 THEN
        RAISE EXCEPTION 'effective organization must have exactly one owner';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER organizations_single_owner
AFTER INSERT OR UPDATE OF status ON organizations
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION enforce_single_effective_owner();
CREATE CONSTRAINT TRIGGER memberships_single_owner
AFTER UPDATE OF status ON memberships
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION enforce_single_effective_owner();
CREATE CONSTRAINT TRIGGER role_bindings_single_owner
AFTER INSERT OR UPDATE OR DELETE ON role_bindings
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION enforce_single_effective_owner();

DO $$
BEGIN
    IF EXISTS (
        SELECT organization.id
        FROM organizations organization
        LEFT JOIN memberships membership ON membership.organization_id=organization.id AND membership.status<>'REVOKED'
        LEFT JOIN role_bindings binding ON binding.membership_id=membership.id AND binding.role='OWNER'
            AND binding.revoked_at IS NULL AND binding.starts_at<=now() AND (binding.expires_at IS NULL OR binding.expires_at>now())
        WHERE organization.status<>'DELETED'
        GROUP BY organization.id HAVING count(binding.id)<>1
    ) THEN
        RAISE EXCEPTION 'existing effective organizations must have exactly one owner before migration';
    END IF;
END;
$$;
