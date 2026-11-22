UPDATE sms_challenges
SET code_hash = decode(repeat('00', 32), 'hex')
WHERE code_hash IS NULL;

COMMENT ON COLUMN sms_challenges.code_hash IS NULL;

ALTER TABLE sms_challenges
    ALTER COLUMN code_hash SET NOT NULL;
