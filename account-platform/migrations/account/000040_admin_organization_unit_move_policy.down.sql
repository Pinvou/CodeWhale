DROP TRIGGER approval_command_policies_immutable ON approval_command_policies;
DELETE FROM approval_command_policies WHERE command_type='ACCOUNT_UNIT_MOVE' AND policy_version=1;
CREATE TRIGGER approval_command_policies_immutable BEFORE UPDATE OR DELETE OR TRUNCATE ON approval_command_policies FOR EACH STATEMENT EXECUTE FUNCTION protect_approval_policy();
