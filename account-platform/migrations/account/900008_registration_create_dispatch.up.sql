ALTER TABLE registration_operations ADD COLUMN identity_create_attempted_at timestamptz;

-- Stop/drain old writers before upgrading. A previous creating state may have
-- sent a request whose acknowledgement was lost; absence is never first proof.
UPDATE registration_operations SET identity_create_attempted_at=updated_at
WHERE status IN ('IDENTITY_CREATING','IDENTITY_CREATED','ACCOUNT_COMMITTING','REPAIR_REQUIRED','PROVISIONING','ACCOUNT_COMMITTED') OR identity_subject IS NOT NULL;
