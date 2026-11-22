DROP TABLE token_audit_logs;

ALTER TABLE provider_credentials
    DROP COLUMN validation_checked_by,
    DROP COLUMN validation_status;
