-- Protect all audit actions. Keep the installed trigger identities so this
-- upgrade also covers deployments that already applied migration 000017.
CREATE OR REPLACE FUNCTION protect_security_audit_log() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit evidence is immutable' USING ERRCODE = '55000';
END;
$$;
