DROP INDEX chargeback_cases_wallet_idx;
ALTER TABLE member_quota_grants DROP CONSTRAINT member_quota_grants_suspension_reason_check;
ALTER TABLE member_quota_grants ADD CONSTRAINT member_quota_grants_suspension_reason_check
    CHECK (suspension_reason IN ('MEMBER_REMOVED','ACCOUNT_CLOSED'));
DROP TABLE chargeback_cases;
ALTER TABLE entitlement_packages DROP CONSTRAINT entitlement_packages_payment_source_identity_unique;
DROP TRIGGER receivable_allocations_immutable ON receivable_allocations;
DROP FUNCTION reject_receivable_allocation_mutation();
DROP TABLE receivable_allocations;
DROP INDEX receivables_wallet_open_idx;
DROP TABLE receivables;

ALTER TABLE payment_evidence DROP CONSTRAINT payment_evidence_evidence_type_check;
ALTER TABLE payment_evidence ADD CONSTRAINT payment_evidence_evidence_type_check
    CHECK (evidence_type IN ('CALLBACK','QUERY','BILL','MANUAL_REPAIR'));

DELETE FROM ledger_accounts WHERE id IN (
    '00000000-0000-7000-8000-000000000005',
    '00000000-0000-7000-8000-000000000006'
);
ALTER TABLE journal_entries DROP CONSTRAINT journal_entries_entry_type_check;
ALTER TABLE journal_entries ADD CONSTRAINT journal_entries_entry_type_check
    CHECK (entry_type IN ('RECHARGE','USAGE','REFUND','REVERSAL','ADJUSTMENT','EXPIRE','REPAIR_CREDIT'));
ALTER TABLE ledger_accounts DROP CONSTRAINT ledger_accounts_account_type_check;
ALTER TABLE ledger_accounts ADD CONSTRAINT ledger_accounts_account_type_check
    CHECK (account_type IN ('WALLET_LIABILITY','PAYMENT_CLEARING','USAGE_REVENUE','REFUND_CLEARING','ADJUSTMENT'));

ALTER TABLE wallets
    DROP COLUMN financial_freeze_reason,
    DROP COLUMN financial_status,
    DROP COLUMN receivable_microcredits;
