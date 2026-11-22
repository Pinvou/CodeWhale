INSERT INTO approval_command_policies(
    command_type,policy_version,approval_mode,requester_permission,
    risk_class,approval_count,self_approval_forbidden,snapshot_schema_version,
    critical_resource_fields,expires_after,execution_window,executor_identity,created_at
) VALUES
    ('BILLING_PRICE_DRAFT_CREATE',1,'AUDIT_ONLY','platform.pricing.manage','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
    ('BILLING_PRICE_DRAFT_UPDATE',1,'AUDIT_ONLY','platform.pricing.manage','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
    ('BILLING_PRICE_PUBLISH',1,'AUDIT_ONLY','platform.pricing.manage','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
    ('BILLING_PRICE_TRANSITION',1,'AUDIT_ONLY','platform.pricing.manage','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
    ('BILLING_METERING_UNIT_CREATE',1,'AUDIT_ONLY','platform.pricing.manage','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
    ('BILLING_METERING_UNIT_DEACTIVATE',1,'AUDIT_ONLY','platform.pricing.manage','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now());
