SET ROLE billing_service;

DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM registration_bonus_claims)
       OR EXISTS (SELECT 1 FROM registration_bonus_policy_commands)
       OR EXISTS (SELECT 1 FROM registration_bonus_policy_audit)
       OR EXISTS (SELECT 1 FROM registration_bonus_policy_versions WHERE revision <> 1) THEN
        RAISE EXCEPTION 'registration bonus claims or policy changes exist; destructive rollback is forbidden';
    END IF;
END $$;
DROP TABLE registration_bonus_receipts;
DROP TABLE registration_bonus_claims;
DROP TABLE registration_bonus_policy_audit;
DROP TABLE registration_bonus_policy_commands;
DROP TABLE registration_bonus_policy_state;
DROP TABLE registration_bonus_policy_versions;
