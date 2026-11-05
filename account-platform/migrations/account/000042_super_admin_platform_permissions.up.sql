UPDATE roles
SET description='Platform super administrator with all persistent platform management permissions; secret values remain unavailable and unredacted PII export requires a separate expiring role.'
WHERE code='PLATFORM_SUPER_ADMIN';

INSERT INTO role_permissions(role_code,permission_code)
SELECT 'PLATFORM_SUPER_ADMIN',code
FROM permissions
WHERE scope='PLATFORM'
  AND code<>'platform.audit.pii.export'
ON CONFLICT DO NOTHING;

-- Expanding a role changes every effective assignment without changing the
-- assignment rows themselves. Invalidate platform authorization caches now.
SELECT bump_role_assignment_scope_version(user_id,role_code,organization_id,unit_id)
FROM role_assignments
WHERE revoked_at IS NULL AND role_code='PLATFORM_SUPER_ADMIN';
