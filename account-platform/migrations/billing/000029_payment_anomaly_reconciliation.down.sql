DROP TRIGGER reconciliation_cases_protect_evidence ON reconciliation_cases;
DROP FUNCTION protect_reconciliation_case_evidence();
DROP TRIGGER reconciliation_case_commands_immutable ON reconciliation_case_commands;
DROP FUNCTION reject_reconciliation_case_command_mutation();
DROP TABLE reconciliation_case_commands;
DROP INDEX reconciliation_cases_owner_status_idx;

ALTER TABLE reconciliation_cases
    DROP COLUMN updated_at,
    DROP COLUMN version,
    DROP COLUMN manual_review_case_id,
    DROP COLUMN ledger_snapshot,
    DROP COLUMN refund_snapshot,
    DROP COLUMN review_result,
    DROP COLUMN repair_action,
    DROP COLUMN owner_id,
    DROP COLUMN severity,
    DROP CONSTRAINT reconciliation_cases_status_check,
    DROP CONSTRAINT reconciliation_cases_case_type_check,
    ADD CONSTRAINT reconciliation_cases_status_check CHECK (status IN ('OPEN','RESOLVED','IGNORED')),
    ADD CONSTRAINT reconciliation_cases_case_type_check CHECK (case_type IN ('PROVIDER_PAID_INTERNAL_MISSING','INTERNAL_PAID_PROVIDER_MISSING','AMOUNT_MISMATCH','DUPLICATE_TRANSACTION'));

DROP INDEX reconciliation_runs_scheduled_once_idx;
DROP INDEX reconciliation_runs_history_idx;
DROP TRIGGER reconciliation_provider_rows_immutable ON reconciliation_provider_rows;
DROP FUNCTION reject_reconciliation_provider_row_mutation();
DROP TABLE reconciliation_provider_rows;
ALTER TABLE provider_bill_import_rows
    DROP COLUMN provider_reference_id,
    DROP COLUMN entry_type;
ALTER TABLE reconciliation_runs
    DROP COLUMN input_digest,
    DROP COLUMN requested_by,
    DROP COLUMN source_mode,
    DROP COLUMN provider_version,
    DROP COLUMN provider_id,
    ADD CONSTRAINT reconciliation_runs_channel_business_date_key UNIQUE(channel,business_date);

DROP TRIGGER payment_anomaly_commands_immutable ON payment_anomaly_commands;
DROP FUNCTION reject_payment_anomaly_command_mutation();
DROP TABLE payment_anomaly_commands;
DROP TRIGGER payment_anomaly_cases_protect_evidence ON payment_anomaly_cases;
DROP FUNCTION prevent_payment_review_evidence_mutation();
DROP INDEX payment_anomaly_cases_order_idx;
DROP INDEX payment_anomaly_cases_queue_idx;
DROP TABLE payment_anomaly_cases;

ALTER TABLE forced_refund_cases
    DROP COLUMN version,
    DROP COLUMN revoked_bonus_microcredits,
    DROP COLUMN revoked_base_microcredits,
    DROP COLUMN receivable_id,
    DROP COLUMN resolution_reason,
    DROP COLUMN resolution_code,
    DROP COLUMN manual_review_case_id;

ALTER TABLE chargeback_cases
    DROP COLUMN version,
    DROP COLUMN resolution_reason,
    DROP COLUMN resolution_code,
    DROP COLUMN manual_review_case_id,
    DROP CONSTRAINT chargeback_cases_status_check,
    ADD CONSTRAINT chargeback_cases_status_check CHECK (status IN ('CONFIRMED','RESOLVED'));

DELETE FROM manual_review_case_events WHERE case_id IN (
    SELECT id FROM manual_review_cases WHERE domain='BILLING' AND case_type IN ('PAYMENT_ANOMALY','RECONCILIATION_DIFFERENCE')
);
DELETE FROM manual_review_cases WHERE domain='BILLING' AND case_type IN ('PAYMENT_ANOMALY','RECONCILIATION_DIFFERENCE');
