WITH revoked_grants AS (
    DELETE FROM role_permissions
    WHERE permission_code IN ('platform.sms-config.read','platform.sms-config.manage')
      AND role_code IN ('PLATFORM_SUPER_ADMIN','PLATFORM_OPERATIONS_ADMIN','PLATFORM_AUDITOR')
    RETURNING role_code
), changed_bindings AS (
    SELECT DISTINCT assignment.user_id,'PLATFORM'::text AS scope,
           '00000000-0000-0000-0000-000000000000'::uuid AS scope_id
    FROM role_assignments assignment
    JOIN revoked_grants ON revoked_grants.role_code=assignment.role_code
    WHERE assignment.revoked_at IS NULL
      AND assignment.starts_at<=now()
      AND (assignment.expires_at IS NULL OR assignment.expires_at>now())
)
SELECT increment_authorization_scope_version(user_id,scope,scope_id)
FROM changed_bindings;

DELETE FROM permissions WHERE code IN ('platform.sms-config.read','platform.sms-config.manage');
DROP TABLE sms_configuration_state;
DROP TABLE sms_configuration_versions;
DROP FUNCTION reject_sms_configuration_version_mutation();
