DROP TRIGGER users_last_super_admin ON users;
DROP FUNCTION protect_last_active_super_admin_user();
DROP TRIGGER role_assignments_last_super_admin ON role_assignments;
DROP FUNCTION protect_last_active_super_admin_assignment();
DROP FUNCTION assert_not_last_active_super_admin(UUID);
DROP TRIGGER audit_logs_security_no_truncate ON audit_logs;
DROP TRIGGER audit_logs_security_immutable ON audit_logs;
DROP FUNCTION protect_security_audit_log();
DROP TRIGGER security_alerts_immutable ON security_alerts;
DROP TRIGGER break_glass_operations_immutable ON break_glass_operations;
DROP FUNCTION protect_security_evidence();
DROP TABLE security_alerts;
DROP TABLE break_glass_operations;
DROP TABLE super_admin_bootstrap_credentials;
ALTER TABLE users DROP CONSTRAINT users_status_check;
ALTER TABLE users ADD CONSTRAINT users_status_check
    CHECK (status IN ('PROVISIONING', 'ACTIVE', 'REPAIR_REQUIRED', 'DISABLED', 'DELETED'));
