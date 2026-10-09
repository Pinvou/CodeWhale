SET LOCAL lock_timeout = '2s';

ALTER TABLE profile_change_requests
    ADD CONSTRAINT profile_change_requests_status_v2_check
    CHECK (status IN (
        'PENDING', 'VERIFIED', 'IDENTITY_UPDATED', 'ACCOUNT_COMMITTED',
        'RETRY', 'COMPLETED', 'REPAIR_REQUIRED', 'CANCELLED_ACCOUNT_CLOSURE',
        'SUPERSEDED'
    ))
    NOT VALID;
