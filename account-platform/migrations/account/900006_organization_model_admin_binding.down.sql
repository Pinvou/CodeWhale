-- Validation intentionally rejects rollback while any historical binding uses
-- the added role. Never discard, rewrite or revoke a binding to force rollback.
ALTER TABLE role_bindings DROP CONSTRAINT role_bindings_role_check;
ALTER TABLE role_bindings ADD CONSTRAINT role_bindings_role_check
    CHECK (role IN ('OWNER', 'ORG_ADMIN', 'BILLING_ADMIN', 'UNIT_ADMIN', 'MEMBER', 'AUDITOR'));
