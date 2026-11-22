SET ROLE account_service;

ALTER TABLE registration_bonus_eligibility_state
    ADD COLUMN backfill_cursor_mobile_e164 TEXT,
    ADD COLUMN backfill_cursor_user_id UUID,
    ADD COLUMN backfill_snapshot_at TIMESTAMPTZ;

-- Replace only 000087's state/key invariant; its published SQL stays immutable.
ALTER TABLE registration_bonus_eligibility_state
    DROP CONSTRAINT registration_bonus_eligibility_state_check1,
    ADD CONSTRAINT registration_bonus_eligibility_state_key_binding_check CHECK (
        (status = 'PENDING'
         AND fingerprint_key_id IS NULL AND fingerprint_key_verifier IS NULL
         AND recoverable_registration_count = 0
         AND backfill_cursor_mobile_e164 IS NULL
         AND backfill_cursor_user_id IS NULL
         AND backfill_snapshot_at IS NULL)
        OR
        (fingerprint_key_id IS NOT NULL AND length(fingerprint_key_id) BETWEEN 1 AND 100
         AND fingerprint_key_verifier IS NOT NULL AND octet_length(fingerprint_key_verifier) = 32
         AND (status <> 'PENDING'
              OR (recoverable_registration_count > 0
                  AND backfill_cursor_mobile_e164 IS NOT NULL
                  AND backfill_cursor_user_id IS NOT NULL
                  AND backfill_snapshot_at IS NOT NULL)))
    );

DROP TRIGGER registration_bonus_eligibility_state_progression ON registration_bonus_eligibility_state;
DROP TRIGGER registration_bonus_eligibility_state_no_truncate ON registration_bonus_eligibility_state;
DROP FUNCTION protect_registration_bonus_eligibility_state();

CREATE FUNCTION protect_registration_bonus_eligibility_state() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE'
       AND OLD.status = 'PENDING'
       AND NEW.status = 'PENDING'
       AND NEW.singleton = OLD.singleton
       AND NEW.fingerprint_key_id IS NOT NULL
       AND NEW.fingerprint_key_verifier IS NOT NULL
       AND (OLD.fingerprint_key_id IS NULL
            OR (NEW.fingerprint_key_id = OLD.fingerprint_key_id
                AND NEW.fingerprint_key_verifier = OLD.fingerprint_key_verifier))
       AND NEW.completed_at IS NULL
       AND NEW.recoverable_registration_count > OLD.recoverable_registration_count
       AND NEW.unrecoverable_registration_count = OLD.unrecoverable_registration_count
       AND NEW.backfill_cursor_mobile_e164 IS NOT NULL
       AND NEW.backfill_cursor_user_id IS NOT NULL
       AND NEW.backfill_snapshot_at IS NOT NULL
       AND (OLD.backfill_cursor_mobile_e164 IS NULL
            OR (NEW.backfill_cursor_mobile_e164,NEW.backfill_cursor_user_id) > (OLD.backfill_cursor_mobile_e164,OLD.backfill_cursor_user_id))
       AND (OLD.backfill_snapshot_at IS NULL OR NEW.backfill_snapshot_at = OLD.backfill_snapshot_at) THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE'
       AND OLD.status = 'PENDING'
       AND NEW.status IN ('READY','BLOCKED')
       AND NEW.singleton = OLD.singleton
       AND NEW.fingerprint_key_id IS NOT NULL
       AND NEW.fingerprint_key_verifier IS NOT NULL
       AND (OLD.fingerprint_key_id IS NULL
            OR (NEW.fingerprint_key_id = OLD.fingerprint_key_id
                AND NEW.fingerprint_key_verifier = OLD.fingerprint_key_verifier))
       AND NEW.recoverable_registration_count >= OLD.recoverable_registration_count
       AND NEW.completed_at IS NOT NULL
       AND NEW.backfill_cursor_mobile_e164 IS NULL
       AND NEW.backfill_cursor_user_id IS NULL
       AND NEW.backfill_snapshot_at IS NULL THEN
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

COMMENT ON COLUMN registration_bonus_eligibility_state.backfill_cursor_mobile_e164 IS
    'Resumable historical backfill cursor; only populated while readiness is PENDING.';
COMMENT ON COLUMN registration_bonus_eligibility_state.backfill_cursor_user_id IS
    'Tie-breaker for the resumable historical backfill cursor.';
COMMENT ON COLUMN registration_bonus_eligibility_state.backfill_snapshot_at IS
    'Stable upper bound for the historical candidate snapshot; reused across resumed pages.';
