-- Keep usage permissions intact. Financial bill facts require separate grants.
INSERT INTO permissions(code,scope,description) VALUES
('platform.billing.reconciliation.read','PLATFORM','Read financial reconciliation runs and bill evidence.'),
('platform.billing.reconciliation.run','PLATFORM','Fetch or import provider bills for financial reconciliation.'),
('platform.billing.reconciliation.manage','PLATFORM','Claim and manage financial reconciliation differences.');
WITH desired(role_code,permission_codes) AS (VALUES
('PLATFORM_SUPER_ADMIN',ARRAY['platform.billing.reconciliation.read','platform.billing.reconciliation.run','platform.billing.reconciliation.manage']::text[]),
('PLATFORM_BILLING_OPERATOR',ARRAY['platform.billing.reconciliation.read','platform.billing.reconciliation.run','platform.billing.reconciliation.manage']::text[]),
('PLATFORM_BILLING_APPROVER',ARRAY['platform.billing.reconciliation.read']::text[]),
('PLATFORM_AUDITOR',ARRAY['platform.billing.reconciliation.read']::text[])
)
INSERT INTO role_permissions(role_code,permission_code) SELECT role_code,unnest(permission_codes) FROM desired;
-- Published policies remain immutable. Append the corrected current version.
INSERT INTO approval_command_policies(command_type,policy_version,approval_mode,requester_permission,approver_permission,risk_class,approval_count,self_approval_forbidden,snapshot_schema_version,critical_resource_fields,expires_after,execution_window,executor_identity,created_at) VALUES
('BILLING_RECONCILIATION_RUN',2,'AUDIT_ONLY','platform.billing.reconciliation.run',NULL,'GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('BILLING_PROVIDER_BILL_IMPORT',2,'AUDIT_ONLY','platform.billing.reconciliation.run',NULL,'GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('BILLING_RECONCILIATION_CASE_UPDATE',2,'AUDIT_ONLY','platform.billing.reconciliation.manage',NULL,'GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now());
SELECT bump_role_assignment_scope_version(user_id,role_code,organization_id,unit_id)
FROM role_assignments WHERE revoked_at IS NULL AND role_code IN ('PLATFORM_SUPER_ADMIN','PLATFORM_BILLING_OPERATOR','PLATFORM_BILLING_APPROVER','PLATFORM_AUDITOR');
