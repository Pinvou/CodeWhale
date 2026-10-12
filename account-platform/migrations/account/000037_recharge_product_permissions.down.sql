DELETE FROM role_permissions WHERE permission_code LIKE 'platform.products.%';
DELETE FROM permissions WHERE code LIKE 'platform.products.%';

SELECT bump_role_assignment_scope_version(user_id,role_code,organization_id,unit_id)
FROM role_assignments
WHERE revoked_at IS NULL AND role_code IN ('PLATFORM_SUPER_ADMIN','PLATFORM_BILLING_OPERATOR','PLATFORM_PRICING_ADMIN','PLATFORM_AUDITOR');
