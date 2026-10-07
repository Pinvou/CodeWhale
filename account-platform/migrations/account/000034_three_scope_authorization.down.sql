DROP TRIGGER IF EXISTS role_assignments_scope_guard ON role_assignments;
DROP FUNCTION IF EXISTS validate_role_assignment_scope();

DELETE FROM role_assignments WHERE role_code='PERSONAL_USER' AND source='ACCOUNT_LIFECYCLE';
DELETE FROM authorization_versions WHERE scope IN ('PERSONAL','UNIT');

DELETE FROM role_permissions WHERE role_code IN (
    'PERSONAL_USER','PLATFORM_SUPER_ADMIN','PLATFORM_IDENTITY_ADMIN','PLATFORM_ORGANIZATION_ADMIN',
    'PLATFORM_BILLING_OPERATOR','PLATFORM_BILLING_APPROVER','PLATFORM_PRICING_ADMIN','PLATFORM_MODEL_ADMIN',
    'PLATFORM_OPERATIONS_ADMIN','PLATFORM_AUDITOR','OWNER','ORG_ADMIN','BILLING_ADMIN','ORG_MODEL_ADMIN',
    'UNIT_ADMIN','AUDITOR','MEMBER'
);

DELETE FROM roles WHERE code IN ('PERSONAL_USER','ORG_MODEL_ADMIN');
DELETE FROM permissions WHERE scope='PERSONAL' OR code IN ('org.model.invoke','org.grants.reclaim_closed');
INSERT INTO permissions(code,scope,description) VALUES
('model.invoke','ORGANIZATION','Invoke an allowed model.'),
('organization.grant.reclaim_closed','ORGANIZATION','Reclaim an eligible grant isolated by irreversible account closure.');

INSERT INTO role_permissions(role_code,permission_code)
SELECT 'PLATFORM_SUPER_ADMIN',code FROM permissions
WHERE scope='PLATFORM' AND code NOT IN (
    'platform.tokens.read','platform.tokens.manage','platform.tokens.credentials.validate','platform.audit.pii.export',
    'platform.account-closures.read','platform.account-closures.repair',
    'platform.billing.payment-controls.resume.request','platform.billing.payment-controls.resume.approve'
)
UNION ALL SELECT 'PLATFORM_IDENTITY_ADMIN',code FROM permissions WHERE code IN (
    'platform.roles.read','platform.users.read','platform.users.status.manage','platform.users.identity.repair',
    'platform.registrations.repair','platform.profile-changes.repair','platform.account-closures.read','platform.account-closures.repair'
)
UNION ALL SELECT 'PLATFORM_ORGANIZATION_ADMIN',code FROM permissions WHERE code LIKE 'platform.organizations.%' OR code='platform.roles.read'
UNION ALL SELECT 'PLATFORM_BILLING_OPERATOR',code FROM permissions WHERE code IN (
    'platform.billing.wallets.read','platform.billing.ledger.read','platform.billing.orders.sync',
    'platform.billing.repairs.request','platform.billing.adjustments.request','platform.usage.read',
    'platform.billing.payment-controls.resume.request'
)
UNION ALL SELECT 'PLATFORM_BILLING_APPROVER',code FROM permissions WHERE code IN (
    'platform.billing.wallets.read','platform.billing.ledger.read','platform.billing.repairs.approve',
    'platform.billing.adjustments.approve','platform.billing.holds.release','platform.usage.read',
    'platform.billing.payment-controls.resume.approve'
)
UNION ALL SELECT 'PLATFORM_PRICING_ADMIN',code FROM permissions WHERE code IN ('platform.pricing.read','platform.pricing.manage')
UNION ALL SELECT 'PLATFORM_MODEL_ADMIN',code FROM permissions WHERE code IN (
    'platform.models.read','platform.models.sync','platform.models.manage','platform.tokens.read','platform.tokens.manage',
    'platform.tokens.credentials.validate','platform.usage.read','platform.usage.reconcile.retry'
)
UNION ALL SELECT 'PLATFORM_OPERATIONS_ADMIN',code FROM permissions WHERE code IN (
    'platform.system.status.read','platform.events.retry','platform.usage.read','platform.usage.reconcile.retry'
)
UNION ALL SELECT 'PLATFORM_AUDITOR',code FROM permissions WHERE code IN ('platform.audit.read','platform.audit.export','platform.system.status.read')
UNION ALL SELECT 'OWNER',code FROM permissions WHERE scope IN ('ORGANIZATION','UNIT')
UNION ALL SELECT 'ORG_ADMIN',code FROM permissions WHERE code IN (
    'org.profile.read','org.profile.update','org.units.read','org.units.manage','org.members.read','org.members.manage',
    'org.roles.read','org.roles.assign','org.quota.read.self','org.quota.read.all','org.quota.allocate','org.quota.revoke',
    'org.usage.read.self','org.usage.read.all','org.models.read','org.models.manage','org.audit.read','model.invoke'
)
UNION ALL SELECT 'BILLING_ADMIN',code FROM permissions WHERE code IN (
    'org.profile.read','org.wallet.read','org.entitlements.purchase','org.quota.read.self','org.quota.read.all',
    'org.quota.allocate','org.quota.revoke','org.usage.read.self','org.usage.read.all','org.audit.read','model.invoke',
    'organization.grant.reclaim_closed'
)
UNION ALL SELECT 'UNIT_ADMIN',code FROM permissions WHERE code IN (
    'org.units.read','org.units.manage','org.members.read','org.members.manage','org.quota.read.self','org.quota.read.all',
    'org.quota.allocate','org.quota.revoke','org.usage.read.self','org.usage.read.all','org.models.read','model.invoke'
)
UNION ALL SELECT 'MEMBER',code FROM permissions WHERE code IN (
    'org.profile.read','org.units.read','org.members.read','org.roles.read','org.quota.read.self','org.usage.read.self',
    'org.models.read','model.invoke'
)
UNION ALL SELECT 'AUDITOR',code FROM permissions WHERE code IN (
    'org.profile.read','org.units.read','org.members.read','org.roles.read','org.wallet.read','org.quota.read.self',
    'org.quota.read.all','org.usage.read.self','org.usage.read.all','org.models.read','org.audit.read','org.audit.export'
);

-- Rolling back the truth table also invalidates every still-effective binding.
SELECT bump_role_assignment_scope_version(user_id,role_code,organization_id,unit_id)
FROM role_assignments
WHERE revoked_at IS NULL;

CREATE OR REPLACE FUNCTION bump_authorization_version() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE affected_user UUID; affected_org UUID; affected_scope TEXT; affected_scope_id UUID;
BEGIN
    affected_user:=COALESCE(NEW.user_id,OLD.user_id);
    affected_org:=COALESCE(NEW.organization_id,OLD.organization_id);
    affected_scope:=CASE WHEN affected_org IS NULL THEN 'PLATFORM' ELSE 'ORGANIZATION' END;
    affected_scope_id:=COALESCE(affected_org,'00000000-0000-0000-0000-000000000000'::uuid);
    INSERT INTO authorization_versions(user_id,scope,scope_id,version,updated_at)
    VALUES(affected_user,affected_scope,affected_scope_id,2,now())
    ON CONFLICT(user_id,scope,scope_id) DO UPDATE
    SET version=authorization_versions.version+1,updated_at=EXCLUDED.updated_at;
    IF TG_OP='DELETE' THEN RETURN OLD; END IF;
    RETURN NEW;
END;
$$;

DROP FUNCTION IF EXISTS bump_role_assignment_scope_version(UUID,TEXT,UUID,UUID);
DROP FUNCTION IF EXISTS increment_authorization_scope_version(UUID,TEXT,UUID);

ALTER TABLE authorization_versions DROP CONSTRAINT authorization_versions_scope_id_check;
ALTER TABLE authorization_versions DROP CONSTRAINT authorization_versions_scope_check;
ALTER TABLE authorization_versions ADD CONSTRAINT authorization_versions_scope_check CHECK (scope IN ('PLATFORM','ORGANIZATION'));
ALTER TABLE authorization_versions ADD CONSTRAINT authorization_versions_check
    CHECK ((scope='PLATFORM')=(scope_id='00000000-0000-0000-0000-000000000000'::uuid));

ALTER TABLE roles DROP CONSTRAINT roles_scope_check;
ALTER TABLE roles ADD CONSTRAINT roles_scope_check CHECK (scope IN ('PLATFORM','ORGANIZATION','UNIT'));
ALTER TABLE permissions DROP CONSTRAINT permissions_scope_check;
ALTER TABLE permissions ADD CONSTRAINT permissions_scope_check CHECK (scope IN ('PLATFORM','ORGANIZATION','UNIT'));
