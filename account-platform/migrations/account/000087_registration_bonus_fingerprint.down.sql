SET ROLE account_service;

DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM registration_bonus_mobile_eligibility)
       OR EXISTS (SELECT 1 FROM registration_bonus_eligibility_state WHERE status <> 'PENDING') THEN
        RAISE EXCEPTION 'registration bonus eligibility facts exist; destructive rollback is forbidden';
    END IF;
END $$;

DROP TRIGGER approval_command_policies_immutable ON approval_command_policies;
DELETE FROM approval_command_policies
WHERE command_type='BILLING_REGISTRATION_BONUS_POLICY_UPDATE' AND policy_version=1;
CREATE TRIGGER approval_command_policies_immutable BEFORE UPDATE OR DELETE OR TRUNCATE ON approval_command_policies
FOR EACH STATEMENT EXECUTE FUNCTION protect_approval_policy();

WITH removed AS (
    DELETE FROM role_permissions
    WHERE permission_code IN ('platform.registration-bonus.read','platform.registration-bonus.manage')
    RETURNING role_code
), changed_bindings AS (
    SELECT DISTINCT assignment.user_id,'PLATFORM'::text AS scope,
           '00000000-0000-0000-0000-000000000000'::uuid AS scope_id
    FROM role_assignments assignment JOIN removed ON removed.role_code=assignment.role_code
    WHERE assignment.revoked_at IS NULL
)
SELECT increment_authorization_scope_version(user_id,scope,scope_id) FROM changed_bindings;
DELETE FROM permissions WHERE code IN ('platform.registration-bonus.read','platform.registration-bonus.manage');

ALTER TABLE wallet_provisioning_jobs
    DROP CONSTRAINT wallet_provisioning_registration_bonus_fingerprint_check,
    DROP COLUMN registration_bonus_fingerprint,
    DROP COLUMN registration_bonus_fingerprint_key_id,
    DROP COLUMN registration_bonus_mode;

DROP TRIGGER registration_bonus_mobile_eligibility_immutable ON registration_bonus_mobile_eligibility;
DROP FUNCTION reject_registration_bonus_eligibility_mutation();
DROP TABLE registration_bonus_mobile_eligibility;
DROP TRIGGER registration_bonus_eligibility_state_progression ON registration_bonus_eligibility_state;
DROP TRIGGER registration_bonus_eligibility_state_no_truncate ON registration_bonus_eligibility_state;
DROP FUNCTION protect_registration_bonus_eligibility_state();
DROP TABLE registration_bonus_eligibility_state;
DROP TRIGGER users_registration_bonus_capable_writer ON users;
DROP FUNCTION require_registration_bonus_capable_writer();
