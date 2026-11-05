ALTER TABLE password_reset_operations
    DROP CONSTRAINT password_reset_operations_proof_check,
    DROP CONSTRAINT password_reset_operations_method_check,
    DROP COLUMN method;

DELETE FROM password_reset_operations
WHERE sms_challenge_id IS NULL;

ALTER TABLE password_reset_operations
    ALTER COLUMN sms_challenge_id SET NOT NULL;
