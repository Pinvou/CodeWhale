ALTER TABLE chargeback_cases DROP CONSTRAINT chargeback_cases_status_check;
ALTER TABLE chargeback_cases
    ADD CONSTRAINT chargeback_cases_status_check CHECK (status IN ('REVIEW_REQUIRED','CONFIRMED','DISMISSED')),
    ADD COLUMN manual_review_case_id UUID UNIQUE REFERENCES manual_review_cases(id),
    ADD COLUMN resolution_code TEXT,
    ADD COLUMN resolution_reason TEXT,
    ADD COLUMN version BIGINT NOT NULL DEFAULT 1 CHECK (version>0);

ALTER TABLE forced_refund_cases
    ADD COLUMN manual_review_case_id UUID UNIQUE REFERENCES manual_review_cases(id),
    ADD COLUMN resolution_code TEXT,
    ADD COLUMN resolution_reason TEXT,
    ADD COLUMN receivable_id UUID REFERENCES receivables(id),
    ADD COLUMN revoked_base_microcredits BIGINT NOT NULL DEFAULT 0 CHECK (revoked_base_microcredits>=0),
    ADD COLUMN revoked_bonus_microcredits BIGINT NOT NULL DEFAULT 0 CHECK (revoked_bonus_microcredits>=0),
    ADD COLUMN version BIGINT NOT NULL DEFAULT 1 CHECK (version>0);

CREATE TABLE payment_anomaly_cases (
    id UUID PRIMARY KEY REFERENCES manual_review_cases(id),
    anomaly_type TEXT NOT NULL CHECK (anomaly_type IN (
        'CHARGEBACK','FORCED_REFUND','DUPLICATE_PAYMENT','LATE_PAYMENT_CONFLICT',
        'REFUND_FAILURE','PAYMENT_FACT_CONFLICT','ILLEGAL_TRANSITION'
    )),
    dedupe_key TEXT NOT NULL UNIQUE,
    payment_order_id UUID REFERENCES payment_orders(id),
    payment_transaction_id UUID REFERENCES payment_transactions(id),
    payment_evidence_id UUID REFERENCES payment_evidence(id),
    refund_id UUID REFERENCES refunds(id),
    forced_refund_case_id UUID UNIQUE REFERENCES forced_refund_cases(id),
    chargeback_case_id UUID UNIQUE REFERENCES chargeback_cases(id),
    status TEXT NOT NULL CHECK (status IN ('OPEN','RESOLVED')),
    resolution_code TEXT,
    resolution_reason TEXT,
    financial_action_status TEXT NOT NULL DEFAULT 'NONE'
        CHECK (financial_action_status IN ('NONE','PENDING','APPLIED','FAILED')),
    resource_snapshot JSONB NOT NULL,
    version BIGINT NOT NULL DEFAULT 1 CHECK (version>0),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    resolved_at TIMESTAMPTZ,
    CHECK ((status='RESOLVED')=(resolved_at IS NOT NULL)),
    CHECK (status='OPEN' OR resolution_code IS NOT NULL)
);

CREATE INDEX payment_anomaly_cases_queue_idx
    ON payment_anomaly_cases(anomaly_type,status,created_at,id)
    WHERE status='OPEN';
CREATE INDEX payment_anomaly_cases_order_idx
    ON payment_anomaly_cases(payment_order_id,created_at,id);

CREATE OR REPLACE FUNCTION prevent_payment_review_evidence_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.anomaly_type<>OLD.anomaly_type
       OR NEW.dedupe_key<>OLD.dedupe_key
       OR NEW.payment_order_id IS DISTINCT FROM OLD.payment_order_id
       OR NEW.payment_transaction_id IS DISTINCT FROM OLD.payment_transaction_id
       OR NEW.payment_evidence_id IS DISTINCT FROM OLD.payment_evidence_id
       OR NEW.refund_id IS DISTINCT FROM OLD.refund_id
       OR NEW.forced_refund_case_id IS DISTINCT FROM OLD.forced_refund_case_id
       OR NEW.chargeback_case_id IS DISTINCT FROM OLD.chargeback_case_id
       OR NEW.resource_snapshot IS DISTINCT FROM OLD.resource_snapshot
       OR NEW.created_at<>OLD.created_at THEN
        RAISE EXCEPTION 'payment anomaly evidence is immutable';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER payment_anomaly_cases_protect_evidence BEFORE UPDATE ON payment_anomaly_cases
FOR EACH ROW EXECUTE FUNCTION prevent_payment_review_evidence_mutation();

CREATE TABLE payment_anomaly_commands (
    id UUID PRIMARY KEY,
    payment_anomaly_case_id UUID NOT NULL REFERENCES payment_anomaly_cases(id),
    actor_id UUID NOT NULL,
    idempotency_key TEXT NOT NULL,
    request_hash TEXT NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    action TEXT NOT NULL,
    reason TEXT NOT NULL,
    before_snapshot JSONB NOT NULL,
    after_snapshot JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE(payment_anomaly_case_id,idempotency_key)
);

CREATE OR REPLACE FUNCTION reject_payment_anomaly_command_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'payment anomaly commands are immutable';
END $$;
CREATE TRIGGER payment_anomaly_commands_immutable BEFORE UPDATE OR DELETE ON payment_anomaly_commands
FOR EACH ROW EXECUTE FUNCTION reject_payment_anomaly_command_mutation();

ALTER TABLE reconciliation_runs DROP CONSTRAINT reconciliation_runs_channel_business_date_key;
ALTER TABLE reconciliation_runs
    ADD COLUMN provider_id TEXT NOT NULL DEFAULT 'legacy',
    ADD COLUMN provider_version TEXT NOT NULL DEFAULT 'legacy',
    ADD COLUMN source_mode TEXT NOT NULL DEFAULT 'MANUAL_API'
        CHECK (source_mode IN ('SCHEDULED_API','MANUAL_API','FILE_IMPORT')),
    ADD COLUMN requested_by UUID,
    ADD COLUMN input_digest TEXT CHECK (input_digest IS NULL OR input_digest ~ '^[0-9a-f]{64}$');

ALTER TABLE reconciliation_runs
    ALTER COLUMN provider_id DROP DEFAULT,
    ALTER COLUMN provider_version DROP DEFAULT,
    ALTER COLUMN source_mode DROP DEFAULT;

CREATE INDEX reconciliation_runs_history_idx
    ON reconciliation_runs(provider_id,provider_version,channel,business_date,started_at,id);
CREATE UNIQUE INDEX reconciliation_runs_scheduled_once_idx
    ON reconciliation_runs(provider_id,provider_version,channel,business_date)
    WHERE source_mode='SCHEDULED_API';

ALTER TABLE provider_bill_import_rows
    ADD COLUMN entry_type TEXT NOT NULL DEFAULT 'PAYMENT'
        CHECK (entry_type IN ('PAYMENT','REFUND','CHARGEBACK')),
    ADD COLUMN provider_reference_id TEXT;

ALTER TABLE provider_bill_import_rows ALTER COLUMN entry_type DROP DEFAULT;

CREATE TABLE reconciliation_provider_rows (
    reconciliation_run_id UUID NOT NULL REFERENCES reconciliation_runs(id),
    row_number INTEGER NOT NULL CHECK (row_number>0),
    entry_type TEXT NOT NULL CHECK (entry_type IN ('PAYMENT','REFUND','CHARGEBACK')),
    provider_reference_id TEXT NOT NULL,
    channel_transaction_id TEXT,
    merchant_order_no TEXT NOT NULL,
    channel TEXT NOT NULL,
    amount_fen BIGINT NOT NULL CHECK (amount_fen>0),
    currency TEXT NOT NULL,
    status TEXT NOT NULL,
    paid_at TIMESTAMPTZ NOT NULL,
    raw_snapshot JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY(reconciliation_run_id,row_number)
);

CREATE OR REPLACE FUNCTION reject_reconciliation_provider_row_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'reconciliation provider rows are immutable';
END $$;
CREATE TRIGGER reconciliation_provider_rows_immutable BEFORE UPDATE OR DELETE ON reconciliation_provider_rows
FOR EACH ROW EXECUTE FUNCTION reject_reconciliation_provider_row_mutation();

ALTER TABLE reconciliation_cases DROP CONSTRAINT reconciliation_cases_case_type_check;
ALTER TABLE reconciliation_cases DROP CONSTRAINT reconciliation_cases_status_check;
ALTER TABLE reconciliation_cases
    ADD CONSTRAINT reconciliation_cases_case_type_check CHECK (case_type IN (
        'PROVIDER_PAID_INTERNAL_MISSING','INTERNAL_PAID_PROVIDER_MISSING','AMOUNT_MISMATCH','DUPLICATE_TRANSACTION',
        'PROVIDER_REFUND_INTERNAL_MISSING','INTERNAL_REFUND_PROVIDER_MISSING','REFUND_AMOUNT_MISMATCH',
        'PROVIDER_CHARGEBACK_INTERNAL_MISSING','INTERNAL_CHARGEBACK_PROVIDER_MISSING',
        'PAYMENT_LEDGER_MISSING','REFUND_LEDGER_MISSING','CHARGEBACK_LEDGER_MISSING','UNSUPPORTED_PROVIDER_ENTRY'
    )),
    ADD CONSTRAINT reconciliation_cases_status_check CHECK (status IN ('OPEN','IN_REVIEW','RESOLVED','IGNORED')),
    ADD COLUMN severity TEXT NOT NULL DEFAULT 'P2' CHECK (severity IN ('P0','P1','P2')),
    ADD COLUMN owner_id UUID,
    ADD COLUMN repair_action TEXT,
    ADD COLUMN review_result TEXT,
    ADD COLUMN refund_snapshot JSONB,
    ADD COLUMN ledger_snapshot JSONB,
    ADD COLUMN manual_review_case_id UUID UNIQUE REFERENCES manual_review_cases(id),
    ADD COLUMN version BIGINT NOT NULL DEFAULT 1 CHECK (version>0),
    ADD COLUMN updated_at TIMESTAMPTZ;

UPDATE reconciliation_cases SET updated_at=COALESCE(resolved_at,created_at);
ALTER TABLE reconciliation_cases ALTER COLUMN updated_at SET NOT NULL;

INSERT INTO manual_review_cases(
    id,domain,case_type,resource_type,resource_id,severity,status,created_at,
    claim_due_at,first_response_due_at,mitigation_due_at,resolution_due_at,resolved_at,updated_at
)
SELECT id,'BILLING','RECONCILIATION_DIFFERENCE','RECONCILIATION_CASE',id,'P2',
       CASE WHEN status='OPEN' THEN 'OPEN' ELSE 'RESOLVED' END,created_at,
       created_at+INTERVAL '4 hours',created_at+INTERVAL '8 hours',created_at+INTERVAL '1 day',created_at+INTERVAL '3 days',
       CASE WHEN status='OPEN' THEN NULL ELSE COALESCE(resolved_at,created_at) END,COALESCE(resolved_at,created_at)
FROM reconciliation_cases
ON CONFLICT(domain,case_type,resource_type,resource_id) DO NOTHING;

UPDATE reconciliation_cases SET manual_review_case_id=id WHERE manual_review_case_id IS NULL;

INSERT INTO manual_review_case_events(id,case_id,event_type,details,created_at)
SELECT gen_random_uuid(),id,'MIGRATED',jsonb_build_object('source','reconciliation_cases','status',status),created_at
FROM reconciliation_cases;

CREATE INDEX reconciliation_cases_owner_status_idx
    ON reconciliation_cases(status,owner_id,severity,created_at,id);

CREATE TABLE reconciliation_case_commands (
    id UUID PRIMARY KEY,
    reconciliation_case_id UUID NOT NULL REFERENCES reconciliation_cases(id),
    actor_id UUID NOT NULL,
    idempotency_key TEXT NOT NULL,
    request_hash TEXT NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    action TEXT NOT NULL CHECK (action IN ('CLAIM','ASSIGN','SET_REPAIR_ACTION','RESOLVE','IGNORE','REOPEN')),
    reason TEXT NOT NULL,
    before_snapshot JSONB NOT NULL,
    after_snapshot JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE(reconciliation_case_id,idempotency_key)
);

CREATE OR REPLACE FUNCTION reject_reconciliation_case_command_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'reconciliation case commands are immutable';
END $$;
CREATE TRIGGER reconciliation_case_commands_immutable BEFORE UPDATE OR DELETE ON reconciliation_case_commands
FOR EACH ROW EXECUTE FUNCTION reject_reconciliation_case_command_mutation();

CREATE OR REPLACE FUNCTION protect_reconciliation_case_evidence() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.reconciliation_run_id<>OLD.reconciliation_run_id
       OR NEW.payment_order_id IS DISTINCT FROM OLD.payment_order_id
       OR NEW.merchant_order_no<>OLD.merchant_order_no
       OR NEW.case_type<>OLD.case_type
       OR NEW.severity<>OLD.severity
       OR NEW.provider_snapshot IS DISTINCT FROM OLD.provider_snapshot
       OR NEW.internal_snapshot IS DISTINCT FROM OLD.internal_snapshot
       OR NEW.refund_snapshot IS DISTINCT FROM OLD.refund_snapshot
       OR NEW.ledger_snapshot IS DISTINCT FROM OLD.ledger_snapshot
       OR NEW.manual_review_case_id IS DISTINCT FROM OLD.manual_review_case_id
       OR NEW.created_at<>OLD.created_at THEN
        RAISE EXCEPTION 'reconciliation evidence is immutable';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER reconciliation_cases_protect_evidence BEFORE UPDATE ON reconciliation_cases
FOR EACH ROW EXECUTE FUNCTION protect_reconciliation_case_evidence();

INSERT INTO manual_review_cases(
    id,domain,case_type,resource_type,resource_id,severity,status,created_at,
    claim_due_at,first_response_due_at,mitigation_due_at,resolution_due_at,updated_at
)
SELECT id,'BILLING','PAYMENT_ANOMALY','PAYMENT_ANOMALY',id,'P1','OPEN',created_at,
       created_at+INTERVAL '1 hour',created_at+INTERVAL '2 hours',created_at+INTERVAL '4 hours',created_at+INTERVAL '8 hours',created_at
FROM forced_refund_cases WHERE status='REVIEW_REQUIRED'
ON CONFLICT(domain,case_type,resource_type,resource_id) DO NOTHING;

UPDATE forced_refund_cases SET manual_review_case_id=id WHERE status='REVIEW_REQUIRED' AND manual_review_case_id IS NULL;

INSERT INTO payment_anomaly_cases(
    id,anomaly_type,dedupe_key,payment_order_id,payment_transaction_id,payment_evidence_id,
    forced_refund_case_id,status,resource_snapshot,created_at,updated_at
)
SELECT id,'FORCED_REFUND','forced-refund:'||id::text,payment_order_id,payment_transaction_id,payment_evidence_id,
       id,'OPEN',entitlement_snapshot,created_at,created_at
FROM forced_refund_cases WHERE status='REVIEW_REQUIRED'
ON CONFLICT(dedupe_key) DO NOTHING;

INSERT INTO manual_review_case_events(id,case_id,event_type,details,created_at)
SELECT gen_random_uuid(),id,'MIGRATED',jsonb_build_object('source','forced_refund_cases','status',status),created_at
FROM forced_refund_cases WHERE status='REVIEW_REQUIRED';
