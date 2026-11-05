ALTER TABLE permissions DROP CONSTRAINT permissions_scope_check;
ALTER TABLE permissions ADD CONSTRAINT permissions_scope_check
    CHECK (scope IN ('PERSONAL','PLATFORM','ORGANIZATION','UNIT'));

ALTER TABLE roles DROP CONSTRAINT roles_scope_check;
ALTER TABLE roles ADD CONSTRAINT roles_scope_check
    CHECK (scope IN ('PERSONAL','PLATFORM','ORGANIZATION','UNIT'));

ALTER TABLE authorization_versions DROP CONSTRAINT authorization_versions_scope_check;
ALTER TABLE authorization_versions DROP CONSTRAINT authorization_versions_check;
ALTER TABLE authorization_versions ADD CONSTRAINT authorization_versions_scope_check
    CHECK (scope IN ('PERSONAL','PLATFORM','ORGANIZATION','UNIT'));
ALTER TABLE authorization_versions ADD CONSTRAINT authorization_versions_scope_id_check CHECK (
    (scope IN ('PERSONAL','PLATFORM') AND scope_id='00000000-0000-0000-0000-000000000000'::uuid)
    OR (scope IN ('ORGANIZATION','UNIT') AND scope_id<>'00000000-0000-0000-0000-000000000000'::uuid)
);

INSERT INTO permissions(code,scope,description) VALUES
('personal.session.login','PERSONAL','Establish a client or Portal session after credential verification.'),
('personal.profile.read','PERSONAL','Read the current user profile.'),
('personal.profile.update','PERSONAL','Update the current user profile.'),
('personal.security.manage','PERSONAL','Change or recover the current user password and security settings.'),
('personal.context.switch','PERSONAL','Switch the current user billing and chat context.'),
('personal.account.close.request','PERSONAL','Request closure of the current user account.'),
('personal.account.close.read','PERSONAL','Read closure progress for the current user account.'),
('personal.billing.read','PERSONAL','Read the current user wallet and entitlement state.'),
('personal.recharge.create','PERSONAL','Create a recharge order for the current user wallet.'),
('personal.recharge.manage','PERSONAL','Inspect and converge current user recharge orders.'),
('personal.models.read','PERSONAL','Read models available to the current personal context.'),
('personal.model.invoke','PERSONAL','Invoke a model in the current personal context.'),
('personal.usage.read','PERSONAL','Read model usage for the current personal context.'),
('personal.usage.cancel','PERSONAL','Cancel an eligible current user model request.'),
('personal.invitations.read','PERSONAL','Read invitations matching the current verified identity.'),
('personal.invitations.accept','PERSONAL','Accept an invitation as the current user.'),
('org.model.invoke','ORGANIZATION','Invoke an allowed model in the target organization context.'),
('org.grants.reclaim_closed','ORGANIZATION','Reclaim an eligible grant isolated by irreversible account closure.')
ON CONFLICT(code) DO UPDATE SET scope=EXCLUDED.scope,description=EXCLUDED.description;

INSERT INTO roles(code,scope,assignable,built_in,description) VALUES
('PERSONAL_USER','PERSONAL',FALSE,TRUE,'Default personal capability role maintained by the account lifecycle.'),
('ORG_MODEL_ADMIN','ORGANIZATION',TRUE,TRUE,'Organization model policy administrator without Token Account or credential access.')
ON CONFLICT(code) DO UPDATE SET
    scope=EXCLUDED.scope,assignable=EXCLUDED.assignable,built_in=EXCLUDED.built_in,description=EXCLUDED.description;

DELETE FROM role_permissions WHERE role_code IN (
    'PERSONAL_USER','PLATFORM_SUPER_ADMIN','PLATFORM_IDENTITY_ADMIN','PLATFORM_ORGANIZATION_ADMIN',
    'PLATFORM_BILLING_OPERATOR','PLATFORM_BILLING_APPROVER','PLATFORM_PRICING_ADMIN','PLATFORM_MODEL_ADMIN',
    'PLATFORM_OPERATIONS_ADMIN','PLATFORM_AUDITOR','OWNER','ORG_ADMIN','BILLING_ADMIN','ORG_MODEL_ADMIN',
    'UNIT_ADMIN','AUDITOR','MEMBER'
);

DELETE FROM permissions WHERE code IN ('model.invoke','organization.grant.reclaim_closed');

WITH desired(role_code,permission_codes) AS (VALUES
('PERSONAL_USER',ARRAY[
    'personal.session.login','personal.profile.read','personal.profile.update','personal.security.manage',
    'personal.context.switch','personal.account.close.request','personal.account.close.read','personal.billing.read',
    'personal.recharge.create','personal.recharge.manage','personal.models.read','personal.model.invoke',
    'personal.usage.read','personal.usage.cancel','personal.invitations.read','personal.invitations.accept'
]::text[]),
('PLATFORM_SUPER_ADMIN',ARRAY[
    'platform.roles.read','platform.roles.assign','platform.users.read','platform.users.status.manage',
    'platform.users.identity.repair','platform.registrations.repair','platform.profile-changes.repair',
    'platform.account-closures.read','platform.account-closures.repair','platform.organizations.read',
    'platform.organizations.create','platform.organizations.status.manage','platform.organizations.delete',
    'platform.organizations.owner.transfer','platform.billing.wallets.read','platform.billing.ledger.read',
    'platform.billing.orders.sync','platform.billing.repairs.request','platform.billing.repairs.approve',
    'platform.billing.adjustments.request','platform.billing.adjustments.approve','platform.billing.holds.release',
    'platform.billing.payment-controls.resume.request','platform.billing.payment-controls.resume.approve',
    'platform.pricing.read','platform.pricing.manage','platform.models.read','platform.models.sync',
    'platform.models.manage','platform.usage.read','platform.usage.reconcile.retry','platform.system.status.read',
    'platform.events.retry','platform.audit.read','platform.audit.export'
]::text[]),
('PLATFORM_IDENTITY_ADMIN',ARRAY[
    'platform.roles.read','platform.users.read','platform.users.status.manage','platform.users.identity.repair',
    'platform.registrations.repair','platform.profile-changes.repair','platform.account-closures.read',
    'platform.account-closures.repair'
]::text[]),
('PLATFORM_ORGANIZATION_ADMIN',ARRAY[
    'platform.roles.read','platform.organizations.read','platform.organizations.create',
    'platform.organizations.status.manage','platform.organizations.delete','platform.organizations.owner.transfer'
]::text[]),
('PLATFORM_BILLING_OPERATOR',ARRAY[
    'platform.billing.wallets.read','platform.billing.ledger.read','platform.billing.orders.sync',
    'platform.billing.repairs.request','platform.billing.adjustments.request','platform.billing.holds.release',
    'platform.billing.payment-controls.resume.request','platform.usage.read'
]::text[]),
('PLATFORM_BILLING_APPROVER',ARRAY[
    'platform.billing.wallets.read','platform.billing.ledger.read','platform.billing.repairs.approve',
    'platform.billing.adjustments.approve','platform.billing.payment-controls.resume.approve','platform.usage.read'
]::text[]),
('PLATFORM_PRICING_ADMIN',ARRAY['platform.pricing.read','platform.pricing.manage']::text[]),
('PLATFORM_MODEL_ADMIN',ARRAY[
    'platform.models.read','platform.models.sync','platform.models.manage','platform.tokens.read',
    'platform.tokens.manage','platform.tokens.credentials.validate','platform.usage.read','platform.usage.reconcile.retry'
]::text[]),
('PLATFORM_OPERATIONS_ADMIN',ARRAY[
    'platform.system.status.read','platform.events.retry','platform.usage.read','platform.usage.reconcile.retry'
]::text[]),
('PLATFORM_AUDITOR',ARRAY[
    'platform.roles.read','platform.users.read','platform.organizations.read','platform.billing.wallets.read',
    'platform.billing.ledger.read','platform.pricing.read','platform.models.read','platform.usage.read',
    'platform.system.status.read','platform.audit.read','platform.audit.export'
]::text[]),
('OWNER',ARRAY[
    'org.profile.read','org.profile.update','org.delete.request','org.owner.transfer','org.units.read',
    'org.units.manage','org.members.read','org.members.manage','org.roles.read','org.roles.assign',
    'org.wallet.read','org.entitlements.purchase','org.quota.read.self','org.quota.read.all',
    'org.quota.allocate','org.quota.revoke','org.usage.read.self','org.usage.read.all','org.models.read',
    'org.models.manage','org.audit.read','org.audit.export','org.grants.reclaim_closed','org.model.invoke'
]::text[]),
('ORG_ADMIN',ARRAY[
    'org.profile.read','org.profile.update','org.units.read','org.units.manage','org.members.read',
    'org.members.manage','org.roles.read','org.roles.assign','org.wallet.read','org.entitlements.purchase',
    'org.quota.read.self','org.quota.read.all','org.quota.allocate','org.quota.revoke','org.usage.read.self',
    'org.usage.read.all','org.models.read','org.models.manage','org.audit.read','org.grants.reclaim_closed',
    'org.model.invoke'
]::text[]),
('BILLING_ADMIN',ARRAY[
    'org.profile.read','org.wallet.read','org.entitlements.purchase','org.quota.read.self','org.quota.read.all',
    'org.quota.allocate','org.quota.revoke','org.usage.read.self','org.usage.read.all','org.audit.read',
    'org.grants.reclaim_closed','org.model.invoke'
]::text[]),
('ORG_MODEL_ADMIN',ARRAY[
    'org.profile.read','org.models.read','org.models.manage','org.usage.read.self','org.model.invoke'
]::text[]),
('UNIT_ADMIN',ARRAY[
    'org.units.read','org.units.manage','org.members.read','org.members.manage','org.quota.read.self',
    'org.usage.read.self','org.models.read','org.model.invoke'
]::text[]),
('AUDITOR',ARRAY[
    'org.profile.read','org.units.read','org.members.read','org.roles.read','org.wallet.read',
    'org.quota.read.self','org.quota.read.all','org.usage.read.self','org.usage.read.all','org.models.read',
    'org.audit.read','org.audit.export'
]::text[]),
('MEMBER',ARRAY[
    'org.profile.read','org.roles.read','org.quota.read.self','org.usage.read.self','org.models.read','org.model.invoke'
]::text[])
)
INSERT INTO role_permissions(role_code,permission_code)
SELECT role_code,unnest(permission_codes) FROM desired;

CREATE FUNCTION increment_authorization_scope_version(p_user_id UUID,p_scope TEXT,p_scope_id UUID)
RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO authorization_versions(user_id,scope,scope_id,version,updated_at)
    VALUES(p_user_id,p_scope,p_scope_id,2,now())
    ON CONFLICT(user_id,scope,scope_id) DO UPDATE
    SET version=authorization_versions.version+1,updated_at=EXCLUDED.updated_at;
END;
$$;

CREATE FUNCTION bump_role_assignment_scope_version(p_user_id UUID,p_role_code TEXT,p_organization_id UUID,p_unit_id UUID)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE role_scope TEXT;
BEGIN
    SELECT scope INTO role_scope FROM roles WHERE code=p_role_code;
    IF role_scope='PERSONAL' THEN
        PERFORM increment_authorization_scope_version(p_user_id,'PERSONAL','00000000-0000-0000-0000-000000000000'::uuid);
    ELSIF role_scope='PLATFORM' THEN
        PERFORM increment_authorization_scope_version(p_user_id,'PLATFORM','00000000-0000-0000-0000-000000000000'::uuid);
    ELSIF role_scope='ORGANIZATION' THEN
        PERFORM increment_authorization_scope_version(p_user_id,'ORGANIZATION',p_organization_id);
    ELSIF role_scope='UNIT' THEN
        PERFORM increment_authorization_scope_version(p_user_id,'ORGANIZATION',p_organization_id);
        PERFORM increment_authorization_scope_version(p_user_id,'UNIT',p_unit_id);
    ELSE
        RAISE EXCEPTION 'unknown role scope for %',p_role_code;
    END IF;
END;
$$;

CREATE OR REPLACE FUNCTION bump_authorization_version() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP IN ('UPDATE','DELETE') THEN
        PERFORM bump_role_assignment_scope_version(OLD.user_id,OLD.role_code,OLD.organization_id,OLD.unit_id);
    END IF;
    IF TG_OP IN ('INSERT','UPDATE') THEN
        PERFORM bump_role_assignment_scope_version(NEW.user_id,NEW.role_code,NEW.organization_id,NEW.unit_id);
    END IF;
    IF TG_OP='DELETE' THEN RETURN OLD; END IF;
    RETURN NEW;
END;
$$;

CREATE FUNCTION validate_role_assignment_scope() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE role_scope TEXT;
BEGIN
    SELECT scope INTO role_scope FROM roles WHERE code=NEW.role_code;
    IF role_scope IN ('PERSONAL','PLATFORM') AND (NEW.organization_id IS NOT NULL OR NEW.unit_id IS NOT NULL) THEN
        RAISE EXCEPTION 'role % does not accept organization or unit scope',NEW.role_code;
    ELSIF role_scope='ORGANIZATION' AND (NEW.organization_id IS NULL OR NEW.unit_id IS NOT NULL) THEN
        RAISE EXCEPTION 'role % requires organization scope only',NEW.role_code;
    ELSIF role_scope='UNIT' AND (NEW.organization_id IS NULL OR NEW.unit_id IS NULL) THEN
        RAISE EXCEPTION 'role % requires organization and unit scope',NEW.role_code;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER role_assignments_scope_guard
BEFORE INSERT OR UPDATE OF role_code,organization_id,unit_id ON role_assignments
FOR EACH ROW EXECUTE FUNCTION validate_role_assignment_scope();

-- Replacing role_permissions changes every effective assignment even though the
-- assignment rows themselves did not change. Invalidate all affected caches.
SELECT bump_role_assignment_scope_version(user_id,role_code,organization_id,unit_id)
FROM role_assignments
WHERE revoked_at IS NULL;

INSERT INTO role_assignments(
    id,user_id,role_code,organization_id,unit_id,created_by,source,grant_source,
    starts_at,expires_at,revoked_at,revoked_by,idempotency_key,created_at
)
SELECT md5('personal-role:'||u.id::text)::uuid,u.id,'PERSONAL_USER',NULL,NULL,NULL,
       'ACCOUNT_LIFECYCLE','ACCOUNT_LIFECYCLE',u.created_at,NULL,NULL,NULL,'personal-user-default',u.created_at
FROM users u
WHERE u.status IN ('BOOTSTRAP_PENDING','PROVISIONING','ACTIVE','REPAIR_REQUIRED')
ON CONFLICT DO NOTHING;

INSERT INTO authorization_versions(user_id,scope,scope_id,version,updated_at)
SELECT ra.user_id,'UNIT',ra.unit_id,1,now()
FROM role_assignments ra JOIN roles ro ON ro.code=ra.role_code AND ro.scope='UNIT'
WHERE ra.unit_id IS NOT NULL
ON CONFLICT(user_id,scope,scope_id) DO NOTHING;
