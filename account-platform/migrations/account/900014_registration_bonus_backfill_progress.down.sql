SET ROLE account_service;

DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM registration_bonus_eligibility_state WHERE status <> 'PENDING' OR fingerprint_key_id IS NOT NULL)
       OR EXISTS (SELECT 1 FROM registration_bonus_mobile_eligibility) THEN
        RAISE EXCEPTION 'registration bonus eligibility facts exist; destructive rollback is forbidden';
    END IF;
END $$;

DROP TRIGGER registration_bonus_eligibility_state_progression ON registration_bonus_eligibility_state;
DROP TRIGGER registration_bonus_eligibility_state_no_truncate ON registration_bonus_eligibility_state;
DROP FUNCTION protect_registration_bonus_eligibility_state();
ALTER TABLE registration_bonus_eligibility_state
    DROP CONSTRAINT registration_bonus_eligibility_state_key_binding_check,
    DROP COLUMN backfill_cursor_mobile_e164,
    DROP COLUMN backfill_cursor_user_id,
    DROP COLUMN backfill_snapshot_at;

ALTER TABLE registration_bonus_eligibility_state
    ADD CONSTRAINT registration_bonus_eligibility_state_check1 CHECK (
        (status = 'PENDING') = (fingerprint_key_id IS NULL AND fingerprint_key_verifier IS NULL)
    );

CREATE FUNCTION protect_registration_bonus_eligibility_state() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE'
       AND OLD.status = 'PENDING'
       AND NEW.status IN ('READY','BLOCKED')
       AND NEW.singleton = OLD.singleton
       AND NEW.fingerprint_key_id IS NOT NULL
       AND NEW.fingerprint_key_verifier IS NOT NULL
       AND NEW.completed_at IS NOT NULL THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'registration bonus eligibility readiness is append-only and can transition only once';
END $$;
CREATE TRIGGER registration_bonus_eligibility_state_progression
BEFORE UPDATE OR DELETE ON registration_bonus_eligibility_state
FOR EACH ROW EXECUTE FUNCTION protect_registration_bonus_eligibility_state();
CREATE TRIGGER registration_bonus_eligibility_state_no_truncate
BEFORE TRUNCATE ON registration_bonus_eligibility_state
FOR EACH STATEMENT EXECUTE FUNCTION protect_registration_bonus_eligibility_state();
