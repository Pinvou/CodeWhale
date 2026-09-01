INSERT INTO permissions(code,scope,description) VALUES
('platform.organizations.profile.manage','PLATFORM','Update platform-managed organization profile fields.'),
('platform.organizations.members.manage','PLATFORM','Add, update, or remove organization members from the management plane.'),
('platform.organizations.units.manage','PLATFORM','Create, update, move, or remove organization units from the management plane.')
ON CONFLICT(code) DO UPDATE SET scope=EXCLUDED.scope,description=EXCLUDED.description;

-- Keep every built-in or custom role that previously managed organization
-- lifecycle state fully functional. New grants can split these capabilities.
WITH inherited_roles AS (
    SELECT DISTINCT role_code
    FROM role_permissions
    WHERE permission_code='platform.organizations.status.manage'
), desired(permission_code) AS (VALUES
    ('platform.organizations.profile.manage'),
    ('platform.organizations.members.manage'),
    ('platform.organizations.units.manage')
)
INSERT INTO role_permissions(role_code,permission_code)
SELECT inherited_roles.role_code,desired.permission_code
FROM inherited_roles CROSS JOIN desired
ON CONFLICT DO NOTHING;

SELECT DISTINCT bump_role_assignment_scope_version(
    ra.user_id,ra.role_code,ra.organization_id,ra.unit_id
)
FROM role_assignments ra
JOIN role_permissions rp ON rp.role_code=ra.role_code
WHERE ra.revoked_at IS NULL
  AND rp.permission_code IN (
      'platform.organizations.profile.manage',
      'platform.organizations.members.manage',
      'platform.organizations.units.manage'
  );

-- Approval policies are immutable. Publish version 2 instead of rewriting the
-- historical version 1 rows whose requester permission was status.manage.
INSERT INTO approval_command_policies(
    command_type,policy_version,approval_mode,requester_permission,
    risk_class,approval_count,self_approval_forbidden,snapshot_schema_version,
    critical_resource_fields,expires_after,execution_window,executor_identity,created_at
) VALUES
('ACCOUNT_ORGANIZATION_RENAME',2,'AUDIT_ONLY','platform.organizations.profile.manage','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('ACCOUNT_MEMBER_ADD',2,'AUDIT_ONLY','platform.organizations.members.manage','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('ACCOUNT_MEMBER_ROLES_REPLACE',2,'AUDIT_ONLY','platform.organizations.members.manage','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('ACCOUNT_MEMBER_REMOVE',2,'AUDIT_ONLY','platform.organizations.members.manage','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('ACCOUNT_UNIT_CREATE',2,'AUDIT_ONLY','platform.organizations.units.manage','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('ACCOUNT_UNIT_RENAME',2,'AUDIT_ONLY','platform.organizations.units.manage','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('ACCOUNT_UNIT_DELETE',2,'AUDIT_ONLY','platform.organizations.units.manage','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now());
