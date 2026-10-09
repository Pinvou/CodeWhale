ALTER TABLE break_glass_operations
    ADD COLUMN operator_credential_id TEXT,
    ADD COLUMN incident_ticket TEXT;

-- The immutable evidence trigger is statement-level, so it rejects even an
-- UPDATE that matches zero historical rows. This migration runs in one
-- transaction: suspend the trigger only for the deterministic compatibility
-- backfill, then restore it before making the new columns mandatory.
DROP TRIGGER break_glass_operations_immutable ON break_glass_operations;

UPDATE break_glass_operations
SET operator_credential_id='legacy:' || id::text,
    incident_ticket='LEGACY-MIGRATION'
WHERE operator_credential_id IS NULL OR incident_ticket IS NULL;

CREATE TRIGGER break_glass_operations_immutable
BEFORE UPDATE OR DELETE OR TRUNCATE ON break_glass_operations
FOR EACH STATEMENT EXECUTE FUNCTION protect_security_evidence();

ALTER TABLE break_glass_operations
    ALTER COLUMN operator_credential_id SET NOT NULL,
    ALTER COLUMN incident_ticket SET NOT NULL,
    ADD CONSTRAINT break_glass_operator_credential_id_length CHECK (char_length(operator_credential_id) BETWEEN 8 AND 200),
    ADD CONSTRAINT break_glass_incident_ticket_length CHECK (char_length(incident_ticket) BETWEEN 3 AND 200),
    ADD CONSTRAINT break_glass_operator_credential_unique UNIQUE (operator_credential_id);
