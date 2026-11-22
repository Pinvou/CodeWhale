-- Command recovery and audit evidence is retained once used. A pristine
-- pre-activation install may be rolled back so migration-chain verification
-- remains reversible without deleting a management fact.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM token_command_results) THEN
        RAISE EXCEPTION 'token command results are retained; disable the feature gate instead' USING ERRCODE='55000';
    END IF;
END $$;

DROP TRIGGER token_command_results_no_delete ON token_command_results;
DROP TRIGGER token_command_results_immutable ON token_command_results;
DROP FUNCTION reject_token_command_result_removal();
DROP FUNCTION protect_token_command_result_identity();
DROP TABLE token_command_results;
