SET LOCAL lock_timeout = '2s';

ALTER TABLE profile_change_requests
    VALIDATE CONSTRAINT profile_change_requests_status_v2_check;
