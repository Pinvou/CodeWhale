-- APT-125-R2: describe the already enforced stable protocol UUID without
-- rewriting any historical policy, request, reservation or command result.
INSERT INTO approval_command_policies(
    command_type,policy_version,approval_mode,requester_permission,approver_permission,
    risk_class,approval_count,self_approval_forbidden,snapshot_schema_version,
    critical_resource_fields,expires_after,execution_window,executor_identity,created_at
) VALUES (
    'TOKEN_ROUTE_UPSERT',3,'AUDIT_ONLY','platform.tokens.manage',NULL,
    'GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,3,
    '["token_account_id","expected_account_version","expected_route_version","model_id","protocol_version_id","precheck_fingerprint","request_fingerprint"]',
    INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()
) ON CONFLICT(command_type,policy_version) DO NOTHING;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM approval_command_policies
        WHERE command_type='TOKEN_ROUTE_UPSERT' AND policy_version=3
          AND approval_mode='AUDIT_ONLY' AND requester_permission='platform.tokens.manage'
          AND approver_permission IS NULL AND risk_class='GOVERNANCE_OR_NORMAL_STATE_MACHINE'
          AND approval_count=0 AND self_approval_forbidden=false AND snapshot_schema_version=3
          AND critical_resource_fields='["token_account_id","expected_account_version","expected_route_version","model_id","protocol_version_id","precheck_fingerprint","request_fingerprint"]'::jsonb
          AND expires_after=INTERVAL '24 hours' AND execution_window=INTERVAL '5 minutes'
          AND executor_identity='requesting-domain-service'
    ) THEN
        RAISE EXCEPTION 'TOKEN_ROUTE_UPSERT v3 policy differs from the approved contract' USING ERRCODE='23514';
    END IF;
END $$;
