DO $$
BEGIN
  RAISE EXCEPTION 'ACCOUNT_SMS_SEND_ATTEMPT_PURPOSES_FORWARD_ONLY: retain password-reset and profile-mobile audit records; correct database issues with a new forward migration'
    USING ERRCODE = '55000';
END $$;
