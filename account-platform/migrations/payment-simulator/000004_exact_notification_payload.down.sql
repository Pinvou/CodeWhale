ALTER TABLE simulator_notifications
    ALTER COLUMN payload TYPE JSONB
    USING convert_from(payload, 'UTF8')::jsonb;
