-- Keep the legacy binding mirror consistent with the installed role registry.
-- No grant, seed, permission or historical binding is changed by this repair.
ALTER TABLE role_bindings DROP CONSTRAINT role_bindings_role_check;
ALTER TABLE role_bindings ADD CONSTRAINT role_bindings_role_check
    CHECK (role IN ('OWNER', 'ORG_ADMIN', 'BILLING_ADMIN', 'UNIT_ADMIN', 'MEMBER', 'AUDITOR', 'ORG_MODEL_ADMIN'));
