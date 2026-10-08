-- Forward-only: preserve every historical run, case and financial fact.
DROP INDEX reconciliation_runs_scheduled_once_idx;
CREATE UNIQUE INDEX reconciliation_runs_input_once_idx ON reconciliation_runs(provider_id,provider_version,channel,business_date,source_mode,input_digest) WHERE source_mode IN ('FILE_IMPORT','SCHEDULED_API') AND input_digest IS NOT NULL;
CREATE TABLE reconciliation_row_progress (
 run_id UUID NOT NULL REFERENCES reconciliation_runs(id),row_number INTEGER NOT NULL,
 processed_at TIMESTAMPTZ,PRIMARY KEY(run_id,row_number),
 FOREIGN KEY(run_id,row_number) REFERENCES reconciliation_provider_rows(reconciliation_run_id,row_number)
);
ALTER TABLE reconciliation_cases ADD COLUMN fact_digest TEXT;
CREATE UNIQUE INDEX reconciliation_cases_fact_once_idx ON reconciliation_cases(fact_digest) WHERE fact_digest IS NOT NULL;
CREATE TABLE reconciliation_case_runs (
 run_id UUID NOT NULL REFERENCES reconciliation_runs(id),case_id UUID NOT NULL REFERENCES reconciliation_cases(id),
 PRIMARY KEY(run_id,case_id)
);
INSERT INTO reconciliation_case_runs(run_id,case_id) SELECT reconciliation_run_id,id FROM reconciliation_cases;
ALTER TABLE reconciliation_cases DROP CONSTRAINT reconciliation_cases_case_type_check;
ALTER TABLE reconciliation_cases ADD CONSTRAINT reconciliation_cases_case_type_check CHECK(case_type IN (
 'PROVIDER_PAID_INTERNAL_MISSING','INTERNAL_PAID_PROVIDER_MISSING','AMOUNT_MISMATCH','DUPLICATE_TRANSACTION',
 'PROVIDER_REFUND_INTERNAL_MISSING','INTERNAL_REFUND_PROVIDER_MISSING','REFUND_AMOUNT_MISMATCH',
 'PROVIDER_CHARGEBACK_INTERNAL_MISSING','INTERNAL_CHARGEBACK_PROVIDER_MISSING','UNSUPPORTED_PROVIDER_ENTRY',
 'PAYMENT_LEDGER_MISSING','REFUND_LEDGER_MISSING','CHARGEBACK_LEDGER_MISSING',
 'PAYMENT_LEDGER_INVALID','REFUND_LEDGER_INVALID','CHARGEBACK_LEDGER_INVALID','PAYMENT_PACKAGE_INVALID','REFUND_DATE_UNVERIFIED','CHARGEBACK_DATE_UNVERIFIED'));
CREATE INDEX reconciliation_runs_recovery_idx ON reconciliation_runs(started_at,id) WHERE status IN ('RUNNING','FAILED');

ALTER TABLE reconciliation_provider_rows ADD COLUMN pipeline_evidence_key TEXT, ADD COLUMN pipeline_payload_digest TEXT;

ALTER TABLE reconciliation_runs ADD COLUMN next_attempt_at TIMESTAMPTZ;
CREATE INDEX reconciliation_runs_retry_due_idx ON reconciliation_runs(next_attempt_at,started_at,id) WHERE status IN ('RUNNING','FAILED');

CREATE TABLE reconciliation_observations (
 id UUID PRIMARY KEY,run_id UUID NOT NULL REFERENCES reconciliation_runs(id),facts_digest TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL,UNIQUE(run_id,facts_digest)
);
CREATE TABLE reconciliation_observation_facts (
 observation_id UUID NOT NULL REFERENCES reconciliation_observations(id),row_number INTEGER NOT NULL,
 entry_type TEXT NOT NULL,provider_reference_id TEXT NOT NULL,payment_order_id UUID NOT NULL,
 amount_fen BIGINT NOT NULL,currency TEXT NOT NULL,internal_snapshot JSONB,refund_snapshot JSONB,ledger_snapshot JSONB,
 integrity_case_type TEXT NOT NULL,integrity_digest TEXT NOT NULL,PRIMARY KEY(observation_id,row_number)
);
CREATE TABLE reconciliation_observation_progress (
 observation_id UUID NOT NULL REFERENCES reconciliation_observations(id),fact_digest TEXT NOT NULL,
 case_id UUID NOT NULL REFERENCES reconciliation_cases(id),PRIMARY KEY(observation_id,fact_digest)
);
ALTER TABLE reconciliation_runs ADD COLUMN active_observation_id UUID REFERENCES reconciliation_observations(id);
CREATE FUNCTION reject_reconciliation_observation_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'reconciliation observations are immutable' USING ERRCODE='55000'; END $$;
CREATE TRIGGER reconciliation_observation_immutable BEFORE UPDATE OR DELETE ON reconciliation_observations FOR EACH ROW EXECUTE FUNCTION reject_reconciliation_observation_mutation();
CREATE TRIGGER reconciliation_observation_fact_immutable BEFORE UPDATE OR DELETE ON reconciliation_observation_facts FOR EACH ROW EXECUTE FUNCTION reject_reconciliation_observation_mutation();

CREATE INDEX reconciliation_provider_rows_source_ref_idx ON reconciliation_provider_rows(entry_type,provider_reference_id,merchant_order_no,reconciliation_run_id);

ALTER TABLE reconciliation_observation_facts ADD COLUMN event_date_verified BOOLEAN NOT NULL, ADD COLUMN event_date_conflict BOOLEAN NOT NULL, ADD COLUMN event_date_digest TEXT NOT NULL, ADD COLUMN event_date_snapshot JSONB;
