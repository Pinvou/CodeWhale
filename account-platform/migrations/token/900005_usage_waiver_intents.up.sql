CREATE TABLE usage_waiver_decisions (
 command_reference UUID PRIMARY KEY,
 request_digest CHAR(64) NOT NULL CHECK(request_digest ~ '^[0-9a-f]{64}$'),
 command_snapshot JSONB NOT NULL,
 admission TEXT NOT NULL CHECK(admission IN ('ADMITTED','REJECTED')),
 decision_snapshot JSONB NOT NULL,
 created_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE usage_waiver_intents (
 usage_request_id UUID PRIMARY KEY REFERENCES usage_requests(id),
 command_reference UUID NOT NULL UNIQUE REFERENCES usage_waiver_decisions(command_reference),
 request_digest CHAR(64) NOT NULL CHECK(request_digest ~ '^[0-9a-f]{64}$'),
 command_snapshot JSONB NOT NULL,
 created_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE usage_waiver_receipts (
 command_reference UUID PRIMARY KEY REFERENCES usage_waiver_intents(command_reference),
 result_snapshot JSONB NOT NULL,
 created_at TIMESTAMPTZ NOT NULL
);
CREATE FUNCTION reject_usage_waiver_evidence_mutation() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN RAISE EXCEPTION 'administrator waiver intent and result are immutable' USING ERRCODE='55000'; END $$;
CREATE TRIGGER usage_waiver_intents_immutable BEFORE UPDATE OR DELETE ON usage_waiver_intents
 FOR EACH ROW EXECUTE FUNCTION reject_usage_waiver_evidence_mutation();
CREATE TRIGGER usage_waiver_receipts_immutable BEFORE UPDATE OR DELETE ON usage_waiver_receipts
 FOR EACH ROW EXECUTE FUNCTION reject_usage_waiver_evidence_mutation();

CREATE TRIGGER usage_waiver_decisions_immutable BEFORE UPDATE OR DELETE ON usage_waiver_decisions
 FOR EACH ROW EXECUTE FUNCTION reject_usage_waiver_evidence_mutation();
