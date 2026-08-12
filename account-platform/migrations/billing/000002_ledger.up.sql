CREATE TABLE wallets (
    id UUID PRIMARY KEY,
    owner_type TEXT NOT NULL CHECK (owner_type IN ('PERSONAL', 'ORGANIZATION', 'ORG_UNIT')),
    owner_id UUID NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('ACTIVE', 'FROZEN', 'CLOSED')),
    posted_microcredits BIGINT NOT NULL DEFAULT 0 CHECK (posted_microcredits >= 0),
    reserved_microcredits BIGINT NOT NULL DEFAULT 0 CHECK (reserved_microcredits >= 0),
    version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE (owner_type, owner_id)
);

CREATE TABLE ledger_accounts (
    id UUID PRIMARY KEY,
    wallet_id UUID REFERENCES wallets(id),
    code TEXT NOT NULL UNIQUE,
    account_type TEXT NOT NULL CHECK (account_type IN ('WALLET_LIABILITY', 'PAYMENT_CLEARING', 'USAGE_REVENUE', 'REFUND_CLEARING', 'ADJUSTMENT')),
    created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE journal_entries (
    id UUID PRIMARY KEY,
    entry_type TEXT NOT NULL CHECK (entry_type IN ('RECHARGE', 'USAGE', 'REFUND', 'REVERSAL', 'ADJUSTMENT')),
    business_reference TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE ledger_postings (
    id UUID PRIMARY KEY,
    journal_entry_id UUID NOT NULL REFERENCES journal_entries(id),
    ledger_account_id UUID NOT NULL REFERENCES ledger_accounts(id),
    delta_microcredits BIGINT NOT NULL CHECK (delta_microcredits <> 0),
    credit_lot_id UUID,
    created_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX ledger_postings_entry_idx ON ledger_postings (journal_entry_id);
CREATE INDEX ledger_postings_account_idx ON ledger_postings (ledger_account_id, created_at);

CREATE FUNCTION enforce_balanced_journal_entry() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF (SELECT COALESCE(sum(delta_microcredits), 0) FROM ledger_postings WHERE journal_entry_id = NEW.journal_entry_id) <> 0 THEN
        RAISE EXCEPTION 'journal entry % is not balanced', NEW.journal_entry_id;
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER ledger_postings_balanced
AFTER INSERT OR UPDATE ON ledger_postings
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION enforce_balanced_journal_entry();

CREATE FUNCTION prevent_ledger_mutation() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'posted ledger records are immutable';
END;
$$;

CREATE TRIGGER journal_entries_immutable BEFORE UPDATE OR DELETE ON journal_entries
FOR EACH ROW EXECUTE FUNCTION prevent_ledger_mutation();
CREATE TRIGGER ledger_postings_immutable BEFORE UPDATE OR DELETE ON ledger_postings
FOR EACH ROW EXECUTE FUNCTION prevent_ledger_mutation();

CREATE TABLE credit_lots (
    id UUID PRIMARY KEY,
    wallet_id UUID NOT NULL REFERENCES wallets(id),
    source_type TEXT NOT NULL CHECK (source_type IN ('RECHARGE', 'PROMOTION', 'ADJUSTMENT')),
    source_reference TEXT NOT NULL UNIQUE,
    original_microcredits BIGINT NOT NULL CHECK (original_microcredits > 0),
    remaining_microcredits BIGINT NOT NULL CHECK (remaining_microcredits >= 0 AND remaining_microcredits <= original_microcredits),
    refundable BOOLEAN NOT NULL,
    expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL
);

ALTER TABLE ledger_postings
    ADD CONSTRAINT ledger_postings_credit_lot_fk FOREIGN KEY (credit_lot_id) REFERENCES credit_lots(id);

CREATE TABLE recharge_products (
    product_id TEXT PRIMARY KEY,
    amount_fen BIGINT NOT NULL CHECK (amount_fen > 0),
    credit_microcredits BIGINT NOT NULL CHECK (credit_microcredits > 0),
    display_points BIGINT NOT NULL CHECK (display_points > 0),
    currency TEXT NOT NULL CHECK (currency = 'CNY'),
    channels TEXT[] NOT NULL,
    catalog_version BIGINT NOT NULL CHECK (catalog_version > 0),
    enabled BOOLEAN NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CHECK (cardinality(channels) > 0)
);

INSERT INTO recharge_products(product_id, amount_fen, credit_microcredits, display_points, currency, channels, catalog_version, enabled, created_at) VALUES
('CNY_1', 100, 1000000, 100, 'CNY', ARRAY['UNIONPAY','ALIPAY','WECHAT'], 1, true, now()),
('CNY_10', 1000, 10000000, 1000, 'CNY', ARRAY['UNIONPAY','ALIPAY','WECHAT'], 1, true, now()),
('CNY_100', 10000, 100000000, 10000, 'CNY', ARRAY['UNIONPAY','ALIPAY','WECHAT'], 1, true, now()),
('CNY_200', 20000, 200000000, 20000, 'CNY', ARRAY['UNIONPAY','ALIPAY','WECHAT'], 1, true, now());

CREATE TABLE pricing_versions (
    id UUID PRIMARY KEY,
    model TEXT NOT NULL,
    input_per_million BIGINT NOT NULL CHECK (input_per_million >= 0),
    output_per_million BIGINT NOT NULL CHECK (output_per_million >= 0),
    cached_input_per_million BIGINT NOT NULL CHECK (cached_input_per_million >= 0),
    effective_from TIMESTAMPTZ NOT NULL,
    effective_to TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    CHECK (effective_to IS NULL OR effective_to > effective_from)
);

CREATE UNIQUE INDEX pricing_versions_active_model_idx ON pricing_versions (model) WHERE effective_to IS NULL;

CREATE TABLE usage_holds (
    id UUID PRIMARY KEY,
    usage_request_id UUID NOT NULL UNIQUE,
    wallet_id UUID NOT NULL REFERENCES wallets(id),
    price_version_id UUID REFERENCES pricing_versions(id),
    status TEXT NOT NULL CHECK (status IN ('ACTIVE', 'COMMITTED', 'RELEASED', 'EXPIRED', 'REVIEW')),
    max_microcredits BIGINT NOT NULL CHECK (max_microcredits > 0),
    actual_microcredits BIGINT CHECK (actual_microcredits >= 0),
    usage_snapshot JSONB,
    expires_at TIMESTAMPTZ NOT NULL,
    terminal_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX usage_holds_expiry_idx ON usage_holds (expires_at) WHERE status = 'ACTIVE';

CREATE TABLE idempotency_records (
    scope TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    response_status INTEGER NOT NULL,
    response_body JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (scope, idempotency_key)
);

INSERT INTO ledger_accounts(id, code, account_type, created_at) VALUES
('00000000-0000-7000-8000-000000000001', 'platform:payment-clearing', 'PAYMENT_CLEARING', now()),
('00000000-0000-7000-8000-000000000002', 'platform:usage-revenue', 'USAGE_REVENUE', now()),
('00000000-0000-7000-8000-000000000003', 'platform:refund-clearing', 'REFUND_CLEARING', now()),
('00000000-0000-7000-8000-000000000004', 'platform:adjustment', 'ADJUSTMENT', now());
