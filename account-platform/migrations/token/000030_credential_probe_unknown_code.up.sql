LOCK TABLE token_command_results IN ACCESS EXCLUSIVE MODE;
ALTER TABLE token_command_results
    DROP CONSTRAINT token_command_results_stable_error_code_check,
    ADD CONSTRAINT token_command_results_stable_error_code_check CHECK (
        stable_error_code IS NULL
        OR stable_error_code = 'CREDENTIAL_PROBE_OUTCOME_UNKNOWN'
        OR (
            char_length(stable_error_code) BETWEEN 1 AND 120
            AND stable_error_code ~ '^[A-Z][A-Z0-9_]*$'
            AND stable_error_code !~ '(TOKEN|KEY|SECRET|PASSWORD|CREDENTIAL)'
        )
    );
