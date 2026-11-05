-- APT-123-R3: describe the already enforced owner/cap evidence without
-- rewriting any historical policy, request, reservation or command result.
INSERT INTO approval_command_policies(
    command_type,policy_version,approval_mode,requester_permission,approver_permission,
    risk_class,approval_count,self_approval_forbidden,snapshot_schema_version,
    critical_resource_fields,expires_after,execution_window,executor_identity,created_at
) VALUES (
    'TOKEN_ACCOUNT_CREATE',3,'AUDIT_ONLY','platform.tokens.manage',NULL,
    'GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,3,
    '["owner_type","owner_id","owner_authorization_version","owner_version","owner_identity_digest","owner_account_count","owner_account_cap","owner_account_cap_policy_version","creation_version","precheck_fingerprint","request_fingerprint"]',
    INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()
) ON CONFLICT(command_type,policy_version) DO NOTHING;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM approval_command_policies
        WHERE command_type='TOKEN_ACCOUNT_CREATE' AND policy_version=3
          AND approval_mode='AUDIT_ONLY' AND requester_permission='platform.tokens.manage'
          AND approver_permission IS NULL AND risk_class='GOVERNANCE_OR_NORMAL_STATE_MACHINE'
          AND approval_count=0 AND self_approval_forbidden=false AND snapshot_schema_version=3
          AND critical_resource_fields='["owner_type","owner_id","owner_authorization_version","owner_version","owner_identity_digest","owner_account_count","owner_account_cap","owner_account_cap_policy_version","creation_version","precheck_fingerprint","request_fingerprint"]'::jsonb
          AND expires_after=INTERVAL '24 hours' AND execution_window=INTERVAL '5 minutes'
          AND executor_identity='requesting-domain-service'
    ) THEN
        RAISE EXCEPTION 'TOKEN_ACCOUNT_CREATE v3 policy differs from the approved contract' USING ERRCODE='23514';
    END IF;
END $$;
