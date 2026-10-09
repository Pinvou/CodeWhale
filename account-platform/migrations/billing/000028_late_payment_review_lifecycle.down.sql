DROP INDEX payment_orders_archive_eligible_idx;
DROP INDEX payment_orders_late_boundary_idx;
DROP TABLE payment_provider_controls;
DROP TRIGGER provider_bill_import_rows_immutable ON provider_bill_import_rows;
DROP TRIGGER provider_bill_imports_immutable ON provider_bill_imports;
DROP FUNCTION reject_provider_bill_import_mutation();
DROP TABLE provider_bill_import_rows;
DROP TABLE provider_bill_imports;
DROP TRIGGER late_payment_review_evidence_immutable ON late_payment_review_evidence;
DROP FUNCTION reject_late_payment_review_evidence_mutation();
DROP TABLE late_payment_review_evidence;
DROP TABLE late_payment_review_cases;
DROP TRIGGER manual_review_case_events_immutable ON manual_review_case_events;
DROP FUNCTION reject_manual_review_case_event_mutation();
DROP TABLE manual_review_case_events;
DROP TABLE manual_review_cases;
ALTER TABLE payment_orders
    DROP CONSTRAINT payment_orders_archive_state_check,
    DROP CONSTRAINT payment_orders_archive_time_order_check,
    DROP COLUMN archived_at,
    DROP COLUMN archive_status,
    DROP COLUMN last_authoritative_unpaid_at,
    DROP COLUMN last_successful_query_at,
    DROP COLUMN final_archive_eligible_at,
    DROP COLUMN late_review_boundary_at,
    DROP COLUMN provider_payable_until;
