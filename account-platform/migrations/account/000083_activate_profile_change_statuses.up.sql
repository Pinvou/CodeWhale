SET LOCAL lock_timeout = '2s';

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM pg_constraint
    WHERE conrelid = 'profile_change_requests'::regclass
      AND conname = 'profile_change_requests_status_v2_check'
      AND convalidated
  ) THEN
    RAISE EXCEPTION '000083 requires the validated profile-change status constraint'
      USING ERRCODE = '55000';
  END IF;
END $$;

ALTER TABLE profile_change_requests
    DROP CONSTRAINT profile_change_requests_status_check;

ALTER TABLE profile_change_requests
    RENAME CONSTRAINT profile_change_requests_status_v2_check
    TO profile_change_requests_status_check;
