CREATE TABLE admin_pagination_snapshots (
    id UUID PRIMARY KEY,
    subject_id UUID NOT NULL,
    resource_type TEXT NOT NULL,
    scope_digest TEXT NOT NULL CHECK (scope_digest ~ '^[0-9a-f]{64}$'),
    filter_digest TEXT NOT NULL CHECK (filter_digest ~ '^[0-9a-f]{64}$'),
    cursor_secret BYTEA NOT NULL CHECK (octet_length(cursor_secret) = 32),
    snapshot_as_of TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    member_count INTEGER NOT NULL CHECK (member_count BETWEEN 0 AND 1000000),
    created_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > created_at)
);

CREATE UNIQUE INDEX admin_pagination_snapshots_subject_resource_active_idx
    ON admin_pagination_snapshots(subject_id, resource_type);
CREATE INDEX admin_pagination_snapshots_expiry_idx
    ON admin_pagination_snapshots(expires_at, id);

CREATE TABLE admin_pagination_snapshot_members (
    snapshot_id UUID NOT NULL REFERENCES admin_pagination_snapshots(id) ON DELETE CASCADE,
    -- Position 1,000,000 is an uncommitted overflow sentinel used to return
    -- PAGINATION_FILTER_REQUIRED; the enclosing transaction always rolls it back.
    position INTEGER NOT NULL CHECK (position >= 0 AND position <= 1000000),
    resource_id TEXT NOT NULL,
    PRIMARY KEY (snapshot_id, position),
    UNIQUE (snapshot_id, resource_id)
);

CREATE TABLE admin_pagination_snapshot_tombstones (
    snapshot_id UUID PRIMARY KEY,
    cursor_secret BYTEA NOT NULL CHECK (octet_length(cursor_secret) = 32),
    purge_after TIMESTAMPTZ NOT NULL
);
CREATE INDEX admin_pagination_snapshot_tombstones_purge_idx
    ON admin_pagination_snapshot_tombstones(purge_after);

CREATE TABLE admin_pagination_creation_windows (
    rate_key TEXT NOT NULL,
    window_start TIMESTAMPTZ NOT NULL,
    creation_count INTEGER NOT NULL CHECK (creation_count >= 0),
    PRIMARY KEY (rate_key, window_start)
);
CREATE INDEX admin_pagination_creation_windows_expiry_idx
    ON admin_pagination_creation_windows(window_start);

CREATE INDEX wallets_admin_pagination_idx
    ON wallets(created_at DESC, id DESC);
CREATE INDEX wallets_admin_filter_idx
    ON wallets(owner_type, status, created_at DESC, id DESC);
CREATE INDEX ledger_postings_admin_pagination_idx
    ON ledger_postings(created_at DESC, id DESC);
CREATE INDEX journal_entries_admin_filter_idx
    ON journal_entries(entry_type, created_at DESC, id DESC);
CREATE INDEX receivables_admin_pagination_idx
    ON receivables(created_at DESC, id DESC);
CREATE INDEX receivables_admin_filter_idx
    ON receivables(wallet_id, status, source_type, created_at DESC, id DESC);
CREATE INDEX receivables_admin_status_idx
    ON receivables(status, source_type, created_at DESC, id DESC);
CREATE INDEX payment_orders_admin_pagination_idx
    ON payment_orders(created_at DESC, id DESC);
CREATE INDEX payment_orders_admin_filter_idx
    ON payment_orders(status, provider_id, channel, created_at DESC, id DESC);
CREATE INDEX payment_orders_admin_provider_idx
    ON payment_orders(provider_id, channel, created_at DESC, id DESC);
CREATE INDEX refunds_admin_pagination_idx
    ON refunds(created_at DESC, id DESC);
CREATE INDEX refunds_admin_status_idx
    ON refunds(status, created_at DESC, id DESC);
CREATE INDEX forced_refund_cases_admin_pagination_idx
    ON forced_refund_cases(created_at DESC, id DESC);
CREATE INDEX forced_refund_cases_admin_status_idx
    ON forced_refund_cases(status, created_at DESC, id DESC);
CREATE INDEX manual_adjustments_admin_pagination_idx
    ON manual_adjustments(created_at DESC, id DESC);
CREATE INDEX manual_adjustments_admin_filter_idx
    ON manual_adjustments(wallet_id, status, requested_by, created_at DESC, id DESC);
CREATE INDEX manual_adjustments_admin_status_idx
    ON manual_adjustments(status, created_at DESC, id DESC);
CREATE INDEX usage_holds_admin_pagination_idx
    ON usage_holds(created_at DESC, id DESC);
CREATE INDEX usage_holds_admin_filter_idx
    ON usage_holds(wallet_id, status, model_id, created_at DESC, id DESC);
CREATE INDEX usage_holds_admin_status_idx
    ON usage_holds(status, model_id, created_at DESC, id DESC);
CREATE INDEX late_payment_reviews_admin_pagination_idx
    ON late_payment_review_cases(created_at DESC, id DESC);
CREATE INDEX late_payment_reviews_admin_filter_idx
    ON late_payment_review_cases(provider_id, review_status, created_at DESC, id DESC);
CREATE INDEX late_payment_reviews_admin_status_idx
    ON late_payment_review_cases(review_status, created_at DESC, id DESC);
CREATE INDEX payment_anomalies_admin_pagination_idx
    ON payment_anomaly_cases(created_at DESC, id DESC);
CREATE INDEX payment_anomalies_admin_filter_idx
    ON payment_anomaly_cases(status, anomaly_type, created_at DESC, id DESC);
CREATE INDEX payment_anomalies_admin_type_idx
    ON payment_anomaly_cases(anomaly_type, created_at DESC, id DESC);
CREATE INDEX reconciliation_runs_admin_pagination_idx
    ON reconciliation_runs(started_at DESC, id DESC);
CREATE INDEX reconciliation_runs_admin_filter_idx
    ON reconciliation_runs(provider_id, channel, status, business_date, started_at DESC, id DESC);
CREATE INDEX reconciliation_runs_admin_status_idx
    ON reconciliation_runs(status, business_date, started_at DESC, id DESC);
CREATE INDEX reconciliation_cases_admin_pagination_idx
    ON reconciliation_cases(created_at DESC, id DESC);
CREATE INDEX reconciliation_cases_admin_filter_idx
    ON reconciliation_cases(reconciliation_run_id, status, severity, owner_id, case_type, created_at DESC, id DESC);
CREATE INDEX reconciliation_cases_admin_status_idx
    ON reconciliation_cases(status, created_at DESC, id DESC);
