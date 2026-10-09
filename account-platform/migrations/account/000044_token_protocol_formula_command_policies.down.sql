DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM approval_requests
        WHERE policy_version=1
          AND command_type IN (
              'TOKEN_PROTOCOL_DRAFT_CREATE',
              'TOKEN_PROTOCOL_PUBLISH',
              'TOKEN_FORMULA_DRAFT_CREATE',
              'TOKEN_FORMULA_PUBLISH'
          )
    ) THEN
        RAISE EXCEPTION 'token protocol/formula policies are referenced and cannot be removed'
            USING ERRCODE='55000';
    END IF;
END $$;

DROP TRIGGER approval_command_policies_immutable ON approval_command_policies;
DELETE FROM approval_command_policies
WHERE policy_version=1
  AND command_type IN (
      'TOKEN_PROTOCOL_DRAFT_CREATE',
      'TOKEN_PROTOCOL_PUBLISH',
      'TOKEN_FORMULA_DRAFT_CREATE',
      'TOKEN_FORMULA_PUBLISH'
  );
CREATE TRIGGER approval_command_policies_immutable
BEFORE UPDATE OR DELETE OR TRUNCATE ON approval_command_policies
FOR EACH STATEMENT EXECUTE FUNCTION protect_approval_policy();
