ALTER TABLE break_glass_operations
    DROP CONSTRAINT break_glass_operator_credential_unique,
    DROP CONSTRAINT break_glass_incident_ticket_length,
    DROP CONSTRAINT break_glass_operator_credential_id_length,
    DROP COLUMN incident_ticket,
    DROP COLUMN operator_credential_id;
