SET LOCAL lock_timeout = '2s';

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM pg_constraint
    WHERE conrelid = 'sms_send_attempts'::regclass
      AND conname = 'sms_send_attempts_purpose_v2_check'
      AND convalidated
  ) THEN
    RAISE EXCEPTION '000080 requires the validated SMS send-attempt purpose constraint'
      USING ERRCODE = '55000';
  END IF;
END $$;

ALTER TABLE sms_send_attempts
    DROP CONSTRAINT sms_send_attempts_purpose_check;

ALTER TABLE sms_send_attempts
    RENAME CONSTRAINT sms_send_attempts_purpose_v2_check
    TO sms_send_attempts_purpose_check;
