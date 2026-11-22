DROP TRIGGER role_bindings_single_owner ON role_bindings;
DROP TRIGGER memberships_single_owner ON memberships;
DROP TRIGGER organizations_single_owner ON organizations;
DROP FUNCTION enforce_single_effective_owner();
DROP TRIGGER invitations_identity_guard ON invitations;
DROP FUNCTION protect_invitation_identity();
DROP TRIGGER role_bindings_no_physical_delete ON role_bindings;
DROP TRIGGER membership_commands_immutable ON membership_commands;
DROP FUNCTION protect_membership_governance_history();
DROP TABLE membership_commands;
DROP INDEX invitations_idempotency_idx;
DROP INDEX invitations_pending_target_idx;
CREATE INDEX invitations_pending_email_idx
    ON invitations (email_normalized,expires_at) WHERE status='PENDING';
ALTER TABLE invitations
    DROP CONSTRAINT invitations_request_hash_check,
    DROP CONSTRAINT invitations_idempotency_key_check,
    DROP CONSTRAINT invitations_target_presence_check,
    DROP CONSTRAINT invitations_target_type_check,
    DROP COLUMN revoked_at,
    DROP COLUMN accepted_at,
    DROP COLUMN version,
    DROP COLUMN request_hash,
    DROP COLUMN idempotency_key,
    DROP COLUMN target_hint,
    DROP COLUMN target_digest,
    DROP COLUMN target_type;
ALTER TABLE invitations ALTER COLUMN email_normalized SET NOT NULL;
