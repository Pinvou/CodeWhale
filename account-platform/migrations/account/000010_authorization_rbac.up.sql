CREATE TABLE permissions (
    code TEXT PRIMARY KEY,
    scope TEXT NOT NULL CHECK (scope IN ('PLATFORM', 'ORGANIZATION', 'UNIT')),
    description TEXT NOT NULL
);

CREATE TABLE roles (
    code TEXT PRIMARY KEY,
    scope TEXT NOT NULL CHECK (scope IN ('PLATFORM', 'ORGANIZATION', 'UNIT')),
    assignable BOOLEAN NOT NULL DEFAULT TRUE,
    built_in BOOLEAN NOT NULL DEFAULT TRUE,
    description TEXT NOT NULL
);

CREATE TABLE role_permissions (
    role_code TEXT NOT NULL REFERENCES roles(code) ON DELETE CASCADE,
    permission_code TEXT NOT NULL REFERENCES permissions(code) ON DELETE CASCADE,
    PRIMARY KEY (role_code, permission_code)
);

CREATE TABLE role_assignments (
    id UUID PRIMARY KEY,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role_code TEXT NOT NULL REFERENCES roles(code),
    organization_id UUID REFERENCES organizations(id) ON DELETE CASCADE,
    unit_id UUID REFERENCES organization_units(id) ON DELETE CASCADE,
    created_by UUID REFERENCES users(id),
    source TEXT NOT NULL DEFAULT 'ACCOUNT',
    created_at TIMESTAMPTZ NOT NULL,
    CHECK (organization_id IS NOT NULL OR unit_id IS NULL)
);

CREATE UNIQUE INDEX role_assignments_unique_idx
    ON role_assignments (
        user_id,
        role_code,
        COALESCE(organization_id, '00000000-0000-0000-0000-000000000000'::uuid),
        COALESCE(unit_id, '00000000-0000-0000-0000-000000000000'::uuid)
    );

CREATE INDEX role_assignments_platform_user_idx
    ON role_assignments (user_id, role_code)
    WHERE organization_id IS NULL;

CREATE INDEX role_assignments_organization_user_idx
    ON role_assignments (organization_id, user_id, role_code)
    WHERE organization_id IS NOT NULL;

CREATE TABLE authorization_versions (
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    scope TEXT NOT NULL CHECK (scope IN ('PLATFORM', 'ORGANIZATION')),
    scope_id UUID NOT NULL,
    version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (user_id, scope, scope_id),
    CHECK ((scope = 'PLATFORM') = (scope_id = '00000000-0000-0000-0000-000000000000'::uuid))
);

INSERT INTO permissions(code, scope, description) VALUES
('platform.roles.read', 'PLATFORM', 'Read platform role definitions and assignments.'),
('platform.roles.assign', 'PLATFORM', 'Assign and revoke platform roles.'),
('platform.users.read', 'PLATFORM', 'Read platform users and identities.'),
('platform.users.status.manage', 'PLATFORM', 'Enable, disable, or delete users.'),
('platform.users.identity.repair', 'PLATFORM', 'Repair user identity attributes.'),
('platform.organizations.read', 'PLATFORM', 'Read organizations and memberships.'),
('platform.organizations.create', 'PLATFORM', 'Create top-level organizations.'),
('platform.organizations.status.manage', 'PLATFORM', 'Enable or disable organizations.'),
('platform.organizations.delete', 'PLATFORM', 'Delete organizations.'),
('platform.organizations.owner.transfer', 'PLATFORM', 'Transfer organization ownership.'),
('platform.billing.wallets.read', 'PLATFORM', 'Read wallets and balances.'),
('platform.billing.ledger.read', 'PLATFORM', 'Read immutable ledger entries.'),
('platform.billing.orders.sync', 'PLATFORM', 'Synchronize payment orders.'),
('platform.billing.repairs.request', 'PLATFORM', 'Request billing repairs.'),
('platform.billing.repairs.approve', 'PLATFORM', 'Approve billing repairs.'),
('platform.billing.adjustments.request', 'PLATFORM', 'Request balance adjustments.'),
('platform.billing.adjustments.approve', 'PLATFORM', 'Approve or reject balance adjustments.'),
('platform.billing.holds.release', 'PLATFORM', 'Release usage holds.'),
('platform.pricing.read', 'PLATFORM', 'Read pricing versions.'),
('platform.pricing.manage', 'PLATFORM', 'Create and activate pricing versions.'),
('platform.models.read', 'PLATFORM', 'Read model routing resources.'),
('platform.models.sync', 'PLATFORM', 'Synchronize model routing resources.'),
('platform.models.manage', 'PLATFORM', 'Manage model routing resources.'),
('platform.tokens.read', 'PLATFORM', 'Read platform Token Accounts and routes.'),
('platform.tokens.manage', 'PLATFORM', 'Manage platform Token Accounts and routes.'),
('platform.tokens.credentials.validate', 'PLATFORM', 'Validate provider credentials.'),
('platform.usage.read', 'PLATFORM', 'Read usage requests and reconciliation state.'),
('platform.usage.reconcile.retry', 'PLATFORM', 'Retry usage reconciliation.'),
('platform.system.status.read', 'PLATFORM', 'Read platform system status.'),
('platform.events.retry', 'PLATFORM', 'Retry failed event delivery.'),
('platform.audit.read', 'PLATFORM', 'Read platform audit events.'),
('platform.audit.export', 'PLATFORM', 'Export redacted platform audit events.'),
('platform.audit.pii.export', 'PLATFORM', 'Export audit events containing personal data.'),
('org.profile.read', 'ORGANIZATION', 'Read organization profile.'),
('org.profile.update', 'ORGANIZATION', 'Update organization profile.'),
('org.delete.request', 'ORGANIZATION', 'Request organization deletion.'),
('org.owner.transfer', 'ORGANIZATION', 'Transfer organization ownership.'),
('org.units.read', 'ORGANIZATION', 'Read organization units.'),
('org.units.manage', 'ORGANIZATION', 'Manage organization units.'),
('org.members.read', 'ORGANIZATION', 'Read organization members.'),
('org.members.manage', 'ORGANIZATION', 'Invite, suspend, or remove organization members.'),
('org.roles.read', 'ORGANIZATION', 'Read organization role definitions and assignments.'),
('org.roles.assign', 'ORGANIZATION', 'Assign and revoke organization roles.'),
('org.wallet.read', 'ORGANIZATION', 'Read organization wallet balances.'),
('org.entitlements.purchase', 'ORGANIZATION', 'Purchase organization entitlements.'),
('org.quota.read.self', 'ORGANIZATION', 'Read the caller quota.'),
('org.quota.read.all', 'ORGANIZATION', 'Read all member quotas.'),
('org.quota.allocate', 'ORGANIZATION', 'Allocate member quota.'),
('org.quota.revoke', 'ORGANIZATION', 'Revoke member quota.'),
('org.usage.read.self', 'ORGANIZATION', 'Read the caller usage.'),
('org.usage.read.all', 'ORGANIZATION', 'Read all organization usage.'),
('org.models.read', 'ORGANIZATION', 'Read the organization model policy.'),
('org.models.manage', 'ORGANIZATION', 'Manage the organization model policy.'),
('org.audit.read', 'ORGANIZATION', 'Read organization audit events.'),
('org.audit.export', 'ORGANIZATION', 'Export organization audit events.'),
('model.invoke', 'ORGANIZATION', 'Invoke an allowed model.');

INSERT INTO roles(code, scope, assignable, description) VALUES
('PLATFORM_SUPER_ADMIN', 'PLATFORM', TRUE, 'Platform role and security administrator; no implicit Token Account credential access.'),
('PLATFORM_IDENTITY_ADMIN', 'PLATFORM', TRUE, 'Platform user and identity administrator.'),
('PLATFORM_ORGANIZATION_ADMIN', 'PLATFORM', TRUE, 'Platform organization administrator.'),
('PLATFORM_BILLING_OPERATOR', 'PLATFORM', TRUE, 'Platform billing operations role.'),
('PLATFORM_BILLING_APPROVER', 'PLATFORM', TRUE, 'Platform billing approval role.'),
('PLATFORM_PRICING_ADMIN', 'PLATFORM', TRUE, 'Platform pricing administrator.'),
('PLATFORM_MODEL_ADMIN', 'PLATFORM', TRUE, 'Platform model, Token Account, route, and credential administrator.'),
('PLATFORM_OPERATIONS_ADMIN', 'PLATFORM', TRUE, 'Platform system and event operations role.'),
('PLATFORM_AUDITOR', 'PLATFORM', TRUE, 'Read-only platform audit role.'),
('OWNER', 'ORGANIZATION', FALSE, 'Organization owner.'),
('ORG_ADMIN', 'ORGANIZATION', TRUE, 'Organization administration role.'),
('BILLING_ADMIN', 'ORGANIZATION', TRUE, 'Organization billing and quota role.'),
('UNIT_ADMIN', 'UNIT', TRUE, 'Administration role scoped to one organization unit subtree.'),
('MEMBER', 'ORGANIZATION', FALSE, 'Base organization member role.'),
('AUDITOR', 'ORGANIZATION', TRUE, 'Read-only organization audit role.');

INSERT INTO role_permissions(role_code, permission_code)
SELECT 'PLATFORM_SUPER_ADMIN', code FROM permissions
WHERE scope = 'PLATFORM' AND code NOT IN ('platform.tokens.read', 'platform.tokens.manage', 'platform.tokens.credentials.validate')
UNION ALL SELECT 'PLATFORM_IDENTITY_ADMIN', code FROM permissions WHERE code IN ('platform.roles.read','platform.users.read','platform.users.status.manage','platform.users.identity.repair')
UNION ALL SELECT 'PLATFORM_ORGANIZATION_ADMIN', code FROM permissions WHERE code LIKE 'platform.organizations.%' OR code = 'platform.roles.read'
UNION ALL SELECT 'PLATFORM_BILLING_OPERATOR', code FROM permissions WHERE code IN ('platform.billing.wallets.read','platform.billing.ledger.read','platform.billing.orders.sync','platform.billing.repairs.request','platform.billing.adjustments.request','platform.usage.read')
UNION ALL SELECT 'PLATFORM_BILLING_APPROVER', code FROM permissions WHERE code IN ('platform.billing.wallets.read','platform.billing.ledger.read','platform.billing.repairs.approve','platform.billing.adjustments.approve','platform.billing.holds.release','platform.usage.read')
UNION ALL SELECT 'PLATFORM_PRICING_ADMIN', code FROM permissions WHERE code IN ('platform.pricing.read','platform.pricing.manage')
UNION ALL SELECT 'PLATFORM_MODEL_ADMIN', code FROM permissions WHERE code IN ('platform.models.read','platform.models.sync','platform.models.manage','platform.tokens.read','platform.tokens.manage','platform.tokens.credentials.validate','platform.usage.read','platform.usage.reconcile.retry')
UNION ALL SELECT 'PLATFORM_OPERATIONS_ADMIN', code FROM permissions WHERE code IN ('platform.system.status.read','platform.events.retry','platform.usage.read','platform.usage.reconcile.retry')
UNION ALL SELECT 'PLATFORM_AUDITOR', code FROM permissions WHERE code IN ('platform.audit.read','platform.audit.export','platform.system.status.read')
UNION ALL SELECT 'OWNER', code FROM permissions WHERE scope IN ('ORGANIZATION','UNIT')
UNION ALL SELECT 'ORG_ADMIN', code FROM permissions WHERE code IN ('org.profile.read','org.profile.update','org.units.read','org.units.manage','org.members.read','org.members.manage','org.roles.read','org.roles.assign','org.quota.read.self','org.quota.read.all','org.quota.allocate','org.quota.revoke','org.usage.read.self','org.usage.read.all','org.models.read','org.models.manage','org.audit.read','model.invoke')
UNION ALL SELECT 'BILLING_ADMIN', code FROM permissions WHERE code IN ('org.profile.read','org.wallet.read','org.entitlements.purchase','org.quota.read.self','org.quota.read.all','org.quota.allocate','org.quota.revoke','org.usage.read.self','org.usage.read.all','org.audit.read','model.invoke')
UNION ALL SELECT 'UNIT_ADMIN', code FROM permissions WHERE code IN ('org.units.read','org.units.manage','org.members.read','org.members.manage','org.quota.read.self','org.quota.read.all','org.quota.allocate','org.quota.revoke','org.usage.read.self','org.usage.read.all','org.models.read','model.invoke')
UNION ALL SELECT 'MEMBER', code FROM permissions WHERE code IN ('org.profile.read','org.units.read','org.members.read','org.roles.read','org.quota.read.self','org.usage.read.self','org.models.read','model.invoke')
UNION ALL SELECT 'AUDITOR', code FROM permissions WHERE code IN ('org.profile.read','org.units.read','org.members.read','org.roles.read','org.wallet.read','org.quota.read.self','org.quota.read.all','org.usage.read.self','org.usage.read.all','org.models.read','org.audit.read','org.audit.export');

CREATE FUNCTION bump_authorization_version() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    affected_user UUID;
    affected_org UUID;
    affected_scope TEXT;
    affected_scope_id UUID;
BEGIN
    affected_user := COALESCE(NEW.user_id, OLD.user_id);
    affected_org := COALESCE(NEW.organization_id, OLD.organization_id);
    affected_scope := CASE WHEN affected_org IS NULL THEN 'PLATFORM' ELSE 'ORGANIZATION' END;
    affected_scope_id := COALESCE(affected_org, '00000000-0000-0000-0000-000000000000'::uuid);
    INSERT INTO authorization_versions(user_id, scope, scope_id, version, updated_at)
    VALUES (affected_user, affected_scope, affected_scope_id, 2, now())
    ON CONFLICT (user_id, scope, scope_id) DO UPDATE
    SET version = authorization_versions.version + 1, updated_at = EXCLUDED.updated_at;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER role_assignments_authorization_version
AFTER INSERT OR UPDATE OR DELETE ON role_assignments
FOR EACH ROW EXECUTE FUNCTION bump_authorization_version();

CREATE FUNCTION mirror_legacy_role_binding() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        DELETE FROM role_assignments WHERE id = OLD.id AND source = 'LEGACY_ROLE_BINDING';
        RETURN OLD;
    END IF;
    INSERT INTO role_assignments(id, user_id, role_code, organization_id, unit_id, created_by, source, created_at)
    SELECT NEW.id, m.user_id, NEW.role, m.organization_id, NEW.unit_id, NEW.created_by, 'LEGACY_ROLE_BINDING', NEW.created_at
    FROM memberships m WHERE m.id = NEW.membership_id
    ON CONFLICT (id) DO UPDATE SET
        user_id = EXCLUDED.user_id,
        role_code = EXCLUDED.role_code,
        organization_id = EXCLUDED.organization_id,
        unit_id = EXCLUDED.unit_id,
        created_by = EXCLUDED.created_by;
    RETURN NEW;
END;
$$;

CREATE TRIGGER role_bindings_rbac_mirror
AFTER INSERT OR UPDATE OR DELETE ON role_bindings
FOR EACH ROW EXECUTE FUNCTION mirror_legacy_role_binding();

INSERT INTO role_assignments(id, user_id, role_code, organization_id, unit_id, created_by, source, created_at)
SELECT rb.id, m.user_id, rb.role, m.organization_id, rb.unit_id, rb.created_by, 'LEGACY_ROLE_BINDING', rb.created_at
FROM role_bindings rb JOIN memberships m ON m.id = rb.membership_id
ON CONFLICT DO NOTHING;
