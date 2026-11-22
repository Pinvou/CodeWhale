CREATE OR REPLACE FUNCTION protect_security_audit_log() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'TRUNCATE' OR OLD.action LIKE 'security.%' THEN
        RAISE EXCEPTION 'security audit evidence is immutable' USING ERRCODE = '55000';
    END IF;
    RETURN OLD;
END;
$$;
