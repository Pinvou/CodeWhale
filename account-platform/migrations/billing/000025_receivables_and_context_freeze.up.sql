ALTER TABLE wallets
    ADD COLUMN receivable_microcredits BIGINT NOT NULL DEFAULT 0 CHECK (receivable_microcredits >= 0),
    ADD COLUMN financial_status TEXT NOT NULL DEFAULT 'CLEAR'
        CHECK (financial_status IN ('CLEAR','RECEIVABLE_OPEN')),
    ADD COLUMN financial_freeze_reason TEXT;

ALTER TABLE ledger_accounts DROP CONSTRAINT ledger_accounts_account_type_check;
ALTER TABLE ledger_accounts ADD CONSTRAINT ledger_accounts_account_type_check
    CHECK (account_type IN ('WALLET_LIABILITY','PAYMENT_CLEARING','USAGE_REVENUE','REFUND_CLEARING','ADJUSTMENT','RECEIVABLE_ASSET','CHARGEBACK_CLEARING'));
ALTER TABLE journal_entries DROP CONSTRAINT journal_entries_entry_type_check;
ALTER TABLE journal_entries ADD CONSTRAINT journal_entries_entry_type_check
    CHECK (entry_type IN ('RECHARGE','USAGE','REFUND','REVERSAL','ADJUSTMENT','EXPIRY','RECEIVABLE','CHARGEBACK'));
ALTER TABLE payment_evidence DROP CONSTRAINT payment_evidence_evidence_type_check;
ALTER TABLE payment_evidence ADD CONSTRAINT payment_evidence_evidence_type_check
    CHECK (evidence_type IN ('CALLBACK','QUERY','BILL','MANUAL_REPAIR','CHARGEBACK'));

INSERT INTO ledger_accounts(id,code,account_type,created_at) VALUES
('00000000-0000-7000-8000-000000000005','platform:receivables','RECEIVABLE_ASSET',now()),
('00000000-0000-7000-8000-000000000006','platform:chargeback-clearing','CHARGEBACK_CLEARING',now());

CREATE TABLE receivables (
    id UUID PRIMARY KEY,
    wallet_id UUID NOT NULL REFERENCES wallets(id),
    source_type TEXT NOT NULL CHECK (source_type IN ('USAGE_OVERAGE','CHARGEBACK')),
    source_reference TEXT NOT NULL,
    payment_order_id UUID REFERENCES payment_orders(id),
    payment_transaction_id UUID REFERENCES payment_transactions(id),
    entitlement_package_id UUID REFERENCES entitlement_packages(id),
    usage_request_id UUID,
    original_microcredits BIGINT NOT NULL CHECK (original_microcredits > 0),
    settled_microcredits BIGINT NOT NULL DEFAULT 0 CHECK (settled_microcredits >= 0),
    outstanding_microcredits BIGINT NOT NULL CHECK (outstanding_microcredits >= 0),
    status TEXT NOT NULL CHECK (status IN ('OPEN','PARTIALLY_SETTLED','SETTLED','WRITTEN_OFF')),
    version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    settled_at TIMESTAMPTZ,
    UNIQUE(source_type,source_reference),
    CHECK (settled_microcredits + outstanding_microcredits = original_microcredits),
    CHECK ((status IN ('SETTLED','WRITTEN_OFF')) = (outstanding_microcredits=0)),
    FOREIGN KEY(payment_transaction_id,payment_order_id)
        REFERENCES payment_transactions(id,payment_order_id)
);

CREATE INDEX receivables_wallet_open_idx ON receivables(wallet_id,created_at,id)
    WHERE outstanding_microcredits>0;

CREATE TABLE receivable_allocations (
    id UUID PRIMARY KEY,
    receivable_id UUID NOT NULL REFERENCES receivables(id),
    wallet_id UUID NOT NULL REFERENCES wallets(id),
    source_type TEXT NOT NULL CHECK (source_type IN ('CASH_RECHARGE_BASE','REFUND_RECOVERY','APPROVED_MANUAL_REPAYMENT','APPROVED_WRITE_OFF')),
    source_reference TEXT NOT NULL,
    amount_microcredits BIGINT NOT NULL CHECK (amount_microcredits > 0),
    journal_entry_id UUID NOT NULL REFERENCES journal_entries(id),
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE(receivable_id,source_type,source_reference)
);

CREATE OR REPLACE FUNCTION reject_receivable_allocation_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'receivable allocations are immutable';
END $$;
CREATE TRIGGER receivable_allocations_immutable BEFORE UPDATE OR DELETE ON receivable_allocations
FOR EACH ROW EXECUTE FUNCTION reject_receivable_allocation_mutation();

ALTER TABLE entitlement_packages ADD CONSTRAINT entitlement_packages_payment_source_identity_unique
    UNIQUE(id,source_payment_transaction_id);

CREATE TABLE chargeback_cases (
    id UUID PRIMARY KEY,
    payment_evidence_id UUID NOT NULL UNIQUE REFERENCES payment_evidence(id),
    payment_order_id UUID NOT NULL REFERENCES payment_orders(id),
    payment_transaction_id UUID NOT NULL REFERENCES payment_transactions(id),
    entitlement_package_id UUID NOT NULL REFERENCES entitlement_packages(id),
    wallet_id UUID NOT NULL REFERENCES wallets(id),
    provider_chargeback_id TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('CONFIRMED','RESOLVED')),
    revoked_base_microcredits BIGINT NOT NULL DEFAULT 0 CHECK (revoked_base_microcredits>=0),
    revoked_bonus_microcredits BIGINT NOT NULL DEFAULT 0 CHECK (revoked_bonus_microcredits>=0),
    receivable_id UUID REFERENCES receivables(id),
    created_at TIMESTAMPTZ NOT NULL,
    resolved_at TIMESTAMPTZ,
    UNIQUE(payment_transaction_id,provider_chargeback_id),
    FOREIGN KEY(payment_transaction_id,payment_order_id)
        REFERENCES payment_transactions(id,payment_order_id),
    FOREIGN KEY(entitlement_package_id,payment_transaction_id)
        REFERENCES entitlement_packages(id,source_payment_transaction_id)
);

CREATE INDEX chargeback_cases_wallet_idx ON chargeback_cases(wallet_id,status,created_at,id);
