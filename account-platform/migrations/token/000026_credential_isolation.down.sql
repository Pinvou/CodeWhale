DO $$
BEGIN
    -- Every launch appends an immutable token_audit_logs fact. There is no
    -- safe destructive rollback once even a CREATED launch exists.
    IF EXISTS (SELECT 1 FROM credential_launches)
       OR EXISTS (SELECT 1 FROM token_command_results WHERE credential_probe_claimed_at IS NOT NULL) THEN
        RAISE EXCEPTION 'APT-133 rollback refused: credential isolation history must be retained';
    END IF;
END $$;

DROP TRIGGER credential_isolation_result_immutable ON credential_isolation_results;
DROP FUNCTION reject_credential_result_identity_mutation();
DROP TABLE credential_isolation_results;
DROP TABLE credential_isolation_sessions;
DROP TABLE credential_launches;
ALTER TABLE token_command_results DROP CONSTRAINT token_command_credential_probe_claim_check;
ALTER TABLE token_command_results DROP COLUMN credential_probe_lease_until;
ALTER TABLE token_command_results DROP COLUMN credential_probe_claimed_at;
ALTER TABLE provider_credentials DROP CONSTRAINT provider_credentials_status_check;
ALTER TABLE provider_credentials ADD CONSTRAINT provider_credentials_status_check
    CHECK (status IN ('PENDING','ACTIVE','RETIRED','REVOKED'));
