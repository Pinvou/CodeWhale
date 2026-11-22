ALTER TABLE sms_challenges
    ALTER COLUMN code_hash DROP NOT NULL;

COMMENT ON COLUMN sms_challenges.code_hash IS
    'Legacy locally generated SMS code hash. NULL when the SMS provider generates and verifies the code.';
