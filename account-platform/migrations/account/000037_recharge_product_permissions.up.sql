INSERT INTO permissions(code,scope,description) VALUES
('platform.products.read','PLATFORM','Read recharge product catalog and admission state.'),
('platform.products.create','PLATFORM','Create recharge product drafts.'),
('platform.products.publish','PLATFORM','Verify, publish, renew, or revoke recharge product admission.'),
('platform.products.unpublish','PLATFORM','Remove recharge products from sale.'),
('platform.products.delete','PLATFORM','Logically delete eligible recharge products.')
ON CONFLICT(code) DO UPDATE SET scope=EXCLUDED.scope,description=EXCLUDED.description;

WITH desired(role_code,permission_codes) AS (VALUES
('PLATFORM_SUPER_ADMIN',ARRAY['platform.products.read','platform.products.create','platform.products.publish','platform.products.unpublish','platform.products.delete']::text[]),
('PLATFORM_BILLING_OPERATOR',ARRAY['platform.products.read']::text[]),
('PLATFORM_PRICING_ADMIN',ARRAY['platform.products.read','platform.products.create','platform.products.publish','platform.products.unpublish','platform.products.delete']::text[]),
('PLATFORM_AUDITOR',ARRAY['platform.products.read']::text[])
)
INSERT INTO role_permissions(role_code,permission_code)
SELECT role_code,unnest(permission_codes) FROM desired
ON CONFLICT DO NOTHING;

SELECT bump_role_assignment_scope_version(user_id,role_code,organization_id,unit_id)
FROM role_assignments
WHERE revoked_at IS NULL AND role_code IN ('PLATFORM_SUPER_ADMIN','PLATFORM_BILLING_OPERATOR','PLATFORM_PRICING_ADMIN','PLATFORM_AUDITOR');
