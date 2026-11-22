ALTER TABLE simulator_notifications
    ALTER COLUMN payload TYPE BYTEA
    USING convert_to(payload::text, 'UTF8');
