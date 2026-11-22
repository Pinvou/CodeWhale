-- APT-123 installs the Token governance policy/permission foundation only.
-- Activation state is intentionally absent and is owned by Token migration 000028.
WITH new_permissions(code,description) AS (
    VALUES
      ('platform.models.lifecycle.request','Request a governed model lifecycle transition.'),
      ('platform.models.lifecycle.approve','Approve a governed model lifecycle transition.'),
      ('platform.models.catalog.rollback.request','Request a governed model catalog rollback.'),
      ('platform.models.catalog.rollback.approve','Approve a governed model catalog rollback.'),
      ('platform.tokens.credentials.revoke.request','Request revocation of a Token credential.'),
      ('platform.tokens.credentials.revoke.approve','Approve revocation of a Token credential.')
), existing_permissions AS MATERIALIZED (
    SELECT code,scope FROM permissions
    WHERE code IN (SELECT code FROM new_permissions)
), repaired_permissions AS (
    INSERT INTO permissions(code,scope,description)
    SELECT code,'PLATFORM',description FROM new_permissions
    ON CONFLICT(code) DO UPDATE SET scope=EXCLUDED.scope,description=EXCLUDED.description
    RETURNING code
), scope_repair_roles AS (
    SELECT DISTINCT edge.role_code
    FROM role_permissions edge
    JOIN existing_permissions existing ON existing.code=edge.permission_code
    WHERE existing.scope<>'PLATFORM'
), revoked_grants AS (
    DELETE FROM role_permissions
    WHERE permission_code IN (SELECT code FROM new_permissions)
      AND role_code NOT IN ('PLATFORM_SUPER_ADMIN','PLATFORM_MODEL_ADMIN')
    RETURNING role_code
), inserted_grants AS (
    INSERT INTO role_permissions(role_code,permission_code)
    SELECT role_code,repaired_permissions.code
    FROM (VALUES ('PLATFORM_SUPER_ADMIN'),('PLATFORM_MODEL_ADMIN')) roles(role_code)
    CROSS JOIN repaired_permissions
    ON CONFLICT DO NOTHING
    RETURNING role_code
), changed_roles AS (
    SELECT role_code FROM scope_repair_roles
    UNION SELECT role_code FROM revoked_grants
    UNION SELECT role_code FROM inserted_grants
), changed_bindings AS (
    SELECT assignment.user_id,role_definition.scope AS role_scope,
           assignment.organization_id,assignment.unit_id
    FROM role_assignments assignment
    JOIN changed_roles ON changed_roles.role_code=assignment.role_code
    JOIN roles role_definition ON role_definition.code=assignment.role_code
    WHERE assignment.revoked_at IS NULL
      AND assignment.starts_at<=now()
      AND (assignment.expires_at IS NULL OR assignment.expires_at>now())
), changed_users AS (
    SELECT user_id,'PERSONAL'::text AS scope,
           '00000000-0000-0000-0000-000000000000'::uuid AS scope_id
    FROM changed_bindings WHERE role_scope='PERSONAL'
    UNION
    SELECT user_id,'PLATFORM','00000000-0000-0000-0000-000000000000'::uuid
    FROM changed_bindings WHERE role_scope='PLATFORM'
    UNION
    SELECT user_id,'ORGANIZATION',organization_id
    FROM changed_bindings
    WHERE role_scope IN ('ORGANIZATION','UNIT') AND organization_id IS NOT NULL
    UNION
    SELECT user_id,'UNIT',unit_id
    FROM changed_bindings WHERE role_scope='UNIT' AND unit_id IS NOT NULL
)
SELECT increment_authorization_scope_version(
    user_id,scope,scope_id
) FROM changed_users;

-- Existing v1 rows are immutable. Precise AUDIT_ONLY contracts are forward
-- versions; the three approved exceptions use the exact frozen 7.3 values.
INSERT INTO approval_command_policies(
    command_type,policy_version,approval_mode,requester_permission,
    approver_permission,risk_class,approval_count,self_approval_forbidden,
    snapshot_schema_version,critical_resource_fields,expires_after,
    execution_window,executor_identity,created_at
) VALUES
('TOKEN_MODEL_DRAFT_SAVE',2,'AUDIT_ONLY','platform.models.manage',NULL,'GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,2,'["model_id","expected_catalog_version","expected_draft_version","precheck_fingerprint","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('TOKEN_MODEL_PUBLISH',2,'AUDIT_ONLY','platform.models.manage',NULL,'GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,2,'["model_id","expected_catalog_version","expected_draft_version","precheck_fingerprint","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('TOKEN_MODEL_LIFECYCLE_CHANGE',2,'APPROVAL_REQUIRED','platform.models.lifecycle.request','platform.models.lifecycle.approve','GOVERNANCE_OR_NORMAL_STATE_MACHINE',1,true,2,'["model_id","catalog_version","current_lifecycle","target_lifecycle","lifecycle_plan_version","effective_at","existing_session_cutoff_at","replacement_model_id","billing_price_version","route_set_fingerprint","provisioning_set_fingerprint","inflight_request_set_fingerprint","policy_version"]',INTERVAL '24 hours',INTERVAL '5 minutes','token-command-executor',now()),
('TOKEN_MODEL_CATALOG_SYNC',2,'AUDIT_ONLY','platform.models.sync',NULL,'GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,2,'["expected_catalog_version","adapter_observed_version","catalog_diff_fingerprint","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('TOKEN_MODEL_CATALOG_ROLLBACK',2,'APPROVAL_REQUIRED','platform.models.catalog.rollback.request','platform.models.catalog.rollback.approve','GOVERNANCE_OR_NORMAL_STATE_MACHINE',1,true,2,'["current_catalog_version","source_catalog_version","catalog_diff_fingerprint","billing_price_version","route_set_fingerprint","provisioning_set_fingerprint","inflight_request_set_fingerprint","policy_version"]',INTERVAL '24 hours',INTERVAL '5 minutes','token-command-executor',now()),
('TOKEN_ACCOUNT_CREATE',2,'AUDIT_ONLY','platform.tokens.manage',NULL,'GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,2,'["owner_type","owner_id","owner_authorization_version","creation_version","precheck_fingerprint","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('TOKEN_ACCOUNT_STATUS_CHANGE',2,'AUDIT_ONLY','platform.tokens.manage',NULL,'GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,2,'["token_account_id","expected_account_version","target_status","precheck_fingerprint","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('TOKEN_ROUTE_UPSERT',2,'AUDIT_ONLY','platform.tokens.manage',NULL,'GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,2,'["token_account_id","expected_account_version","expected_route_version","model_id","protocol_code","precheck_fingerprint","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('TOKEN_ROUTE_DELETE',2,'AUDIT_ONLY','platform.tokens.manage',NULL,'GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,2,'["token_account_id","route_id","expected_account_version","expected_route_version","precheck_fingerprint","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('TOKEN_CREDENTIAL_ENROLL',1,'AUDIT_ONLY','platform.tokens.manage',NULL,'GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["token_account_id","expected_account_version","credential_slot_version","launch_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('TOKEN_CREDENTIAL_ROTATE',1,'AUDIT_ONLY','platform.tokens.manage',NULL,'GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["token_account_id","expected_account_version","active_credential_version","launch_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('TOKEN_CREDENTIAL_REVOKE',1,'APPROVAL_REQUIRED','platform.tokens.credentials.revoke.request','platform.tokens.credentials.revoke.approve','GOVERNANCE_OR_NORMAL_STATE_MACHINE',1,true,1,'["token_account_id","token_account_version","credential_id","credential_version","credential_status","credential_fingerprint","active_route_set_fingerprint","provisioning_lease_set_fingerprint","inflight_request_set_fingerprint","replacement_credential_version","policy_version"]',INTERVAL '24 hours',INTERVAL '5 minutes','token-command-executor',now()),
('TOKEN_CREDENTIAL_VALIDATE',2,'AUDIT_ONLY','platform.tokens.credentials.validate',NULL,'GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,2,'["token_account_id","expected_account_version","credential_id","credential_version","precheck_fingerprint","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('TOKEN_ROUTE_PROVISIONING_REPAIR',2,'AUDIT_ONLY','platform.tokens.manage',NULL,'GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,2,'["provisioning_id","expected_provisioning_version","target_generation","precheck_fingerprint","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('TOKEN_USAGE_RECONCILIATION_RETRY',2,'AUDIT_ONLY','platform.usage.reconcile.retry',NULL,'GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,2,'["usage_request_id","expected_usage_version","evidence_high_watermark","precheck_fingerprint","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now())
ON CONFLICT(command_type,policy_version) DO NOTHING;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='approval_executions_stable_error_class') THEN
        ALTER TABLE approval_executions ADD CONSTRAINT approval_executions_stable_error_class CHECK (
            (status = 'FAILED') = (error_class IS NOT NULL)
            AND (
                error_class IS NULL OR (
                    char_length(error_class) BETWEEN 1 AND 120
                    AND error_class ~ '^[A-Z][A-Z0-9_]*$'
                    AND error_class !~ '(TOKEN|KEY|SECRET|PASSWORD|CREDENTIAL)'
                )
            )
        );
    END IF;
END $$;
