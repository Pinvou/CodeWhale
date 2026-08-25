INSERT INTO permissions(code,scope_type,description) VALUES
('platform.billing.payment-controls.resume.request','PLATFORM','Request version-frozen payment Provider recovery.'),
('platform.billing.payment-controls.resume.approve','PLATFORM','Independently approve payment Provider recovery.');

INSERT INTO role_permissions(role_code,permission_code) VALUES
('PLATFORM_BILLING_OPERATOR','platform.billing.payment-controls.resume.request'),
('PLATFORM_BILLING_APPROVER','platform.billing.payment-controls.resume.approve');

INSERT INTO approval_command_policies(command_type,policy_version,approval_mode,requester_permission,approver_permission,risk_class,approval_count,self_approval_forbidden,snapshot_schema_version,critical_resource_fields,expires_after,execution_window,executor_identity,created_at)
VALUES('PAYMENT_PROVIDER_RESUME',1,'APPROVAL_REQUIRED','platform.billing.payment-controls.resume.request','platform.billing.payment-controls.resume.approve','FINANCIAL_FACT_CHANGE',1,true,1,'["provider_id","provider_version","provider_control_version","product_control_versions","probe_order_id","policy_version"]',INTERVAL '24 hours',INTERVAL '5 minutes','billing-command-executor',now());
