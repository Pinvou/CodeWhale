DELETE FROM role_permissions
WHERE role_code='PLATFORM_SUPER_ADMIN'
  AND permission_code IN (
      'platform.tokens.read',
      'platform.tokens.manage',
      'platform.tokens.credentials.validate'
  );

UPDATE roles
SET description='Platform role and security administrator; no implicit Token Account credential access.'
WHERE code='PLATFORM_SUPER_ADMIN';

SELECT bump_role_assignment_scope_version(user_id,role_code,organization_id,unit_id)
FROM role_assignments
WHERE revoked_at IS NULL AND role_code='PLATFORM_SUPER_ADMIN';
