ALTER TABLE break_glass_operations
    ADD COLUMN operator_credential_id TEXT,
    ADD COLUMN incident_ticket TEXT;

UPDATE break_glass_operations
SET operator_credential_id='legacy:' || id::text,
    incident_ticket='LEGACY-MIGRATION'
WHERE operator_credential_id IS NULL OR incident_ticket IS NULL;

ALTER TABLE break_glass_operations
    ALTER COLUMN operator_credential_id SET NOT NULL,
    ALTER COLUMN incident_ticket SET NOT NULL,
    ADD CONSTRAINT break_glass_operator_credential_id_length CHECK (char_length(operator_credential_id) BETWEEN 8 AND 200),
    ADD CONSTRAINT break_glass_incident_ticket_length CHECK (char_length(incident_ticket) BETWEEN 3 AND 200),
    ADD CONSTRAINT break_glass_operator_credential_unique UNIQUE (operator_credential_id);
