DROP TRIGGER approval_command_policies_immutable ON approval_command_policies;
DELETE FROM approval_command_policies
 WHERE (command_type='PAYMENT_PRODUCT_RESUME' AND policy_version=1)
    OR (command_type='PAYMENT_PROVIDER_RESUME' AND policy_version=2);
CREATE TRIGGER approval_command_policies_immutable BEFORE UPDATE OR DELETE OR TRUNCATE ON approval_command_policies FOR EACH STATEMENT EXECUTE FUNCTION protect_approval_policy();
