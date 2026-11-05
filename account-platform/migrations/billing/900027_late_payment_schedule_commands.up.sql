-- A schedule is one actor/case/facts-bound command. Retain its original
-- immutable receipt with the same no-expiry policy as financial case commands.
CREATE TABLE late_payment_review_schedule_commands (
    id UUID PRIMARY KEY,
    case_id UUID NOT NULL REFERENCES late_payment_review_cases(id),
    actor_id UUID NOT NULL,
    idempotency_key TEXT NOT NULL UNIQUE CHECK(length(idempotency_key) BETWEEN 8 AND 120),
    request_hash TEXT NOT NULL CHECK(request_hash ~ '^[0-9a-f]{64}$'),
    request_snapshot JSONB NOT NULL CHECK(jsonb_typeof(request_snapshot)='object'),
    before_snapshot JSONB NOT NULL CHECK(jsonb_typeof(before_snapshot)='object'),
    after_snapshot JSONB NOT NULL CHECK(jsonb_typeof(after_snapshot)='object'),
    created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX late_payment_review_schedule_commands_case_idx
    ON late_payment_review_schedule_commands(case_id,created_at,id);

CREATE FUNCTION reject_late_payment_schedule_command_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'late payment schedule commands and receipts are immutable';
END $$;
CREATE TRIGGER late_payment_schedule_commands_immutable
    BEFORE UPDATE OR DELETE ON late_payment_review_schedule_commands
    FOR EACH ROW EXECUTE FUNCTION reject_late_payment_schedule_command_mutation();
CREATE TRIGGER late_payment_schedule_commands_no_truncate
    BEFORE TRUNCATE ON late_payment_review_schedule_commands
    FOR EACH STATEMENT EXECUTE FUNCTION reject_late_payment_schedule_command_mutation();
