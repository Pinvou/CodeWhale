DROP TRIGGER approval_command_policies_immutable ON approval_command_policies;
DELETE FROM approval_command_policies
WHERE policy_version=1
  AND command_type IN (
      'BILLING_PRICE_DRAFT_CREATE',
      'BILLING_PRICE_DRAFT_UPDATE',
      'BILLING_PRICE_PUBLISH',
      'BILLING_PRICE_TRANSITION',
      'BILLING_METERING_UNIT_CREATE',
      'BILLING_METERING_UNIT_DEACTIVATE'
  );
CREATE TRIGGER approval_command_policies_immutable BEFORE UPDATE OR DELETE OR TRUNCATE ON approval_command_policies FOR EACH STATEMENT EXECUTE FUNCTION protect_approval_policy();
