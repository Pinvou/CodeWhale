DELETE FROM approval_command_policies WHERE command_type='ACCOUNT_CLOSURE_RETRY' AND policy_version=1;
DROP TRIGGER IF EXISTS account_closure_stage_guard ON account_closure_requests;
DROP FUNCTION IF EXISTS enforce_account_closure_stage_progression();
DROP TRIGGER IF EXISTS account_closure_audit_immutable ON account_closure_audit;
DROP TRIGGER IF EXISTS account_closure_commands_immutable ON account_closure_commands;
DROP FUNCTION IF EXISTS reject_account_closure_evidence_mutation();
DROP TABLE IF EXISTS account_closure_audit;
DROP TABLE IF EXISTS account_closure_commands;
DROP TABLE IF EXISTS account_closure_requests;
DELETE FROM role_permissions WHERE permission_code IN ('platform.account-closures.read','platform.account-closures.repair');
DELETE FROM permissions WHERE code IN ('platform.account-closures.read','platform.account-closures.repair');
UPDATE users SET status='DISABLED' WHERE status='CLOSING';
ALTER TABLE users DROP CONSTRAINT users_status_check;
ALTER TABLE users ADD CONSTRAINT users_status_check
    CHECK (status IN ('PROVISIONING','ACTIVE','DISABLED','DELETED','REPAIR_REQUIRED','BOOTSTRAP_PENDING'));
