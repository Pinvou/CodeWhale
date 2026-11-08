ALTER TABLE password_reset_operations
    ALTER COLUMN sms_challenge_id DROP NOT NULL,
    ADD COLUMN method TEXT;

UPDATE password_reset_operations
SET method='SMS'
WHERE method IS NULL;

ALTER TABLE password_reset_operations
    ALTER COLUMN method SET NOT NULL,
    ADD CONSTRAINT password_reset_operations_method_check
        CHECK (method IN ('SMS','AUTHENTICATED')),
    ADD CONSTRAINT password_reset_operations_proof_check
        CHECK ((method='SMS' AND sms_challenge_id IS NOT NULL)
            OR (method='AUTHENTICATED' AND sms_challenge_id IS NULL));
