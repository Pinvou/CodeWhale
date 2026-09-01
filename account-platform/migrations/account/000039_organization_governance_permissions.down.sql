DROP TRIGGER approval_command_policies_immutable ON approval_command_policies;
DELETE FROM approval_command_policies
WHERE policy_version=2 AND command_type IN (
    'ACCOUNT_ORGANIZATION_RENAME',
    'ACCOUNT_MEMBER_ADD','ACCOUNT_MEMBER_ROLES_REPLACE','ACCOUNT_MEMBER_REMOVE',
    'ACCOUNT_UNIT_CREATE','ACCOUNT_UNIT_RENAME','ACCOUNT_UNIT_DELETE'
);
CREATE TRIGGER approval_command_policies_immutable BEFORE UPDATE OR DELETE OR TRUNCATE ON approval_command_policies FOR EACH STATEMENT EXECUTE FUNCTION protect_approval_policy();

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

DELETE FROM role_permissions WHERE permission_code IN (
    'platform.organizations.profile.manage',
    'platform.organizations.members.manage',
    'platform.organizations.units.manage'
);
DELETE FROM permissions WHERE code IN (
    'platform.organizations.profile.manage',
    'platform.organizations.members.manage',
    'platform.organizations.units.manage'
);
