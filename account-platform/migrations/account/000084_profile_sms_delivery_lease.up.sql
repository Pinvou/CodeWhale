ALTER TABLE sms_challenges
    ADD COLUMN delivery_lease_id UUID,
    ADD COLUMN delivery_lease_until TIMESTAMPTZ,
    ADD COLUMN delivery_started_at TIMESTAMPTZ,
    ADD COLUMN delivery_uncertain_at TIMESTAMPTZ,
    ADD CONSTRAINT sms_challenges_delivery_lease_pair_check
        CHECK ((delivery_lease_id IS NULL) = (delivery_lease_until IS NULL)),
    ADD CONSTRAINT sms_challenges_delivery_uncertain_check
        CHECK (delivery_uncertain_at IS NULL OR delivery_started_at IS NOT NULL);

CREATE INDEX sms_challenges_delivery_lease_idx
    ON sms_challenges (delivery_lease_until)
    WHERE status = 'CREATED' AND delivery_started_at IS NOT NULL;
