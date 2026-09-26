DROP INDEX sms_challenges_delivery_lease_idx;

ALTER TABLE sms_challenges
    DROP CONSTRAINT sms_challenges_delivery_uncertain_check,
    DROP CONSTRAINT sms_challenges_delivery_lease_pair_check,
    DROP COLUMN delivery_uncertain_at,
    DROP COLUMN delivery_started_at,
    DROP COLUMN delivery_lease_until,
    DROP COLUMN delivery_lease_id;
