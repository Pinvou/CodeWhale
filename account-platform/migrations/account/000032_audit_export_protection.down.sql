DROP TRIGGER IF EXISTS audit_export_events_immutable ON audit_export_events;
DROP FUNCTION IF EXISTS reject_audit_export_event_mutation();
DROP TABLE IF EXISTS audit_export_events;
DROP TABLE IF EXISTS audit_export_jobs;
DROP TRIGGER approval_command_policies_immutable ON approval_command_policies;
DELETE FROM approval_command_policies
WHERE command_type IN ('ACCOUNT_AUDIT_EXPORT_CREATE','ACCOUNT_AUDIT_EXPORT_DOWNLOAD_TICKET') AND policy_version=1;
CREATE TRIGGER approval_command_policies_immutable BEFORE UPDATE OR DELETE OR TRUNCATE ON approval_command_policies
FOR EACH STATEMENT EXECUTE FUNCTION protect_approval_policy();
DELETE FROM role_permissions WHERE role_code='PLATFORM_AUDIT_PII_EXPORT';
DELETE FROM roles WHERE code='PLATFORM_AUDIT_PII_EXPORT';
