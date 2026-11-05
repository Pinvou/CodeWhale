-- Ordinary member attachment is separate from scoped role authority.
ALTER TABLE invitations ADD COLUMN unit_ids UUID[];
ALTER TABLE membership_commands DROP CONSTRAINT membership_commands_command_type_check;
ALTER TABLE membership_commands ADD CONSTRAINT membership_commands_command_type_check
    CHECK (command_type IN ('ADD','REMOVE','TRANSFER_OWNERSHIP','UNITS_REPLACE'));

CREATE FUNCTION protect_invitation_unit_scope() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.unit_ids IS DISTINCT FROM OLD.unit_ids THEN
        RAISE EXCEPTION 'invitation unit scope is immutable' USING ERRCODE='55000';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER invitation_unit_scope_immutable BEFORE UPDATE OF unit_ids ON invitations
FOR EACH ROW EXECUTE FUNCTION protect_invitation_unit_scope();
