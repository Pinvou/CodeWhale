INSERT INTO approval_command_policies(
    command_type,policy_version,approval_mode,requester_permission,
    approver_permission,risk_class,approval_count,self_approval_forbidden,
    snapshot_schema_version,critical_resource_fields,expires_after,
    execution_window,executor_identity,created_at
) VALUES
    (
        'TOKEN_PROTOCOL_DRAFT_CREATE',1,'AUDIT_ONLY','platform.pricing.manage',
        NULL,'GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,
        '["protocol_code","expected_protocol_head_version","request_fingerprint"]'::jsonb,
        INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()
    ),
    (
        'TOKEN_PROTOCOL_PUBLISH',1,'AUDIT_ONLY','platform.pricing.manage',
        NULL,'GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,
        '["protocol_definition_id","expected_protocol_version","draft_etag","request_fingerprint"]'::jsonb,
        INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()
    ),
    (
        'TOKEN_FORMULA_DRAFT_CREATE',1,'AUDIT_ONLY','platform.pricing.manage',
        NULL,'GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,
        '["protocol_definition_id","expected_protocol_version","expected_formula_head_version","request_fingerprint"]'::jsonb,
        INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()
    ),
    (
        'TOKEN_FORMULA_PUBLISH',1,'AUDIT_ONLY','platform.pricing.manage',
        NULL,'GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,
        '["formula_version_id","expected_formula_version","draft_etag","request_fingerprint"]'::jsonb,
        INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()
    );
