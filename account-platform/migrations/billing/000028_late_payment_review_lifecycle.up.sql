ALTER TABLE payment_orders
    ADD COLUMN provider_payable_until TIMESTAMPTZ,
    ADD COLUMN late_review_boundary_at TIMESTAMPTZ,
    ADD COLUMN final_archive_eligible_at TIMESTAMPTZ,
    ADD COLUMN last_successful_query_at TIMESTAMPTZ,
    ADD COLUMN last_authoritative_unpaid_at TIMESTAMPTZ,
    ADD COLUMN archive_status TEXT NOT NULL DEFAULT 'ACTIVE'
        CHECK (archive_status IN ('ACTIVE','ARCHIVED_CONFIRMED_UNPAID')),
    ADD COLUMN archived_at TIMESTAMPTZ;

UPDATE payment_orders SET
    provider_payable_until=created_at + make_interval(secs => COALESCE(NULLIF(provider_capability_snapshot->>'maximum_payable_window_seconds','')::bigint,900)),
    late_review_boundary_at=created_at + interval '72 hours',
    final_archive_eligible_at=created_at + make_interval(secs => COALESCE(NULLIF(provider_capability_snapshot->>'maximum_payable_window_seconds','')::bigint,900) + COALESCE(NULLIF(provider_capability_snapshot->>'maximum_late_confirmation_seconds','')::bigint,0) + 604800);

ALTER TABLE payment_orders
    ALTER COLUMN provider_payable_until SET NOT NULL,
    ALTER COLUMN late_review_boundary_at SET NOT NULL,
    ALTER COLUMN final_archive_eligible_at SET NOT NULL,
    ADD CONSTRAINT payment_orders_archive_time_order_check
        CHECK (provider_payable_until<=final_archive_eligible_at AND created_at<=late_review_boundary_at),
    ADD CONSTRAINT payment_orders_archive_state_check
        CHECK ((archive_status='ARCHIVED_CONFIRMED_UNPAID')=(archived_at IS NOT NULL));

CREATE TABLE manual_review_cases (
    id UUID PRIMARY KEY,
    domain TEXT NOT NULL,
    case_type TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    resource_id UUID NOT NULL,
    severity TEXT NOT NULL CHECK (severity IN ('P0','P1','P2')),
    status TEXT NOT NULL CHECK (status IN ('OPEN','RESOLVED','LONG_TERM_PLAN')),
    owner_id UUID,
    created_at TIMESTAMPTZ NOT NULL,
    claimed_at TIMESTAMPTZ,
    first_response_at TIMESTAMPTZ,
    claim_due_at TIMESTAMPTZ NOT NULL,
    first_response_due_at TIMESTAMPTZ NOT NULL,
    mitigation_due_at TIMESTAMPTZ NOT NULL,
    resolution_due_at TIMESTAMPTZ NOT NULL,
    mitigation_at TIMESTAMPTZ,
    resolved_at TIMESTAMPTZ,
    escalation_level INTEGER NOT NULL DEFAULT 0 CHECK (escalation_level>=0),
    last_escalated_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE(domain,case_type,resource_type,resource_id),
    CHECK (claimed_at IS NULL OR owner_id IS NOT NULL),
    CHECK (resolved_at IS NULL OR status IN ('RESOLVED','LONG_TERM_PLAN'))
);

CREATE INDEX manual_review_cases_open_sla_idx
    ON manual_review_cases(severity,claim_due_at,first_response_due_at,mitigation_due_at,resolution_due_at,id)
    WHERE status='OPEN';

CREATE TABLE manual_review_case_events (
    id UUID PRIMARY KEY,
    case_id UUID NOT NULL REFERENCES manual_review_cases(id),
    event_type TEXT NOT NULL,
    actor_id UUID,
    details JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX manual_review_case_events_case_idx
    ON manual_review_case_events(case_id,created_at,id);

CREATE OR REPLACE FUNCTION reject_manual_review_case_event_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'manual review case events are immutable';
END $$;
CREATE TRIGGER manual_review_case_events_immutable BEFORE UPDATE OR DELETE ON manual_review_case_events
FOR EACH ROW EXECUTE FUNCTION reject_manual_review_case_event_mutation();

CREATE TABLE late_payment_review_cases (
    id UUID PRIMARY KEY REFERENCES manual_review_cases(id),
    payment_order_id UUID NOT NULL UNIQUE REFERENCES payment_orders(id),
    review_status TEXT NOT NULL CHECK (review_status IN ('REVIEW_REQUIRED_LATE_PAYMENT','PROVIDER_BLIND','ARCHIVED_CONFIRMED_UNPAID','RESOLVED_PAID')),
    provider_id TEXT NOT NULL,
    provider_version TEXT NOT NULL,
    merchant_order_no TEXT NOT NULL,
    provider_capability_snapshot JSONB NOT NULL,
    policy_snapshot JSONB NOT NULL,
    evidence_summary JSONB NOT NULL,
    last_query_at TIMESTAMPTZ,
    last_query_result TEXT,
    next_review_at TIMESTAMPTZ NOT NULL,
    blind_reason TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    resolved_at TIMESTAMPTZ
);

CREATE INDEX late_payment_review_cases_queue_idx
    ON late_payment_review_cases(review_status,next_review_at,created_at,id)
    WHERE review_status IN ('REVIEW_REQUIRED_LATE_PAYMENT','PROVIDER_BLIND');

CREATE TABLE late_payment_review_evidence (
    id UUID PRIMARY KEY,
    case_id UUID NOT NULL REFERENCES late_payment_review_cases(id),
    evidence_type TEXT NOT NULL CHECK (evidence_type IN ('BOUNDARY_QUERY','MANUAL_QUERY','USER_PAYMENT_CLAIM','ARCHIVE_QUERY','PROVIDER_BLIND')),
    payment_evidence_id UUID REFERENCES payment_evidence(id),
    actor_id UUID,
    payload_digest TEXT NOT NULL CHECK (payload_digest ~ '^[0-9a-f]{64}$'),
    payload_snapshot JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE(case_id,evidence_type,payload_digest)
);

CREATE OR REPLACE FUNCTION reject_late_payment_review_evidence_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'late payment review evidence is immutable';
END $$;
CREATE TRIGGER late_payment_review_evidence_immutable BEFORE UPDATE OR DELETE ON late_payment_review_evidence
FOR EACH ROW EXECUTE FUNCTION reject_late_payment_review_evidence_mutation();

CREATE TABLE provider_bill_imports (
    id UUID PRIMARY KEY,
    provider_id TEXT NOT NULL,
    provider_version TEXT NOT NULL,
    channel TEXT NOT NULL,
    business_date DATE NOT NULL,
    payload_digest TEXT NOT NULL CHECK (payload_digest ~ '^[0-9a-f]{64}$'),
    row_count INTEGER NOT NULL CHECK (row_count>=0),
    actor_id UUID NOT NULL,
    reconciliation_run_id UUID REFERENCES reconciliation_runs(id),
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE(provider_id,provider_version,channel,business_date,payload_digest)
);

CREATE TABLE provider_bill_import_rows (
    import_id UUID NOT NULL REFERENCES provider_bill_imports(id),
    row_number INTEGER NOT NULL CHECK (row_number>0),
    merchant_order_no TEXT NOT NULL,
    provider_transaction_id TEXT NOT NULL,
    amount_fen BIGINT NOT NULL CHECK (amount_fen>0),
    currency TEXT NOT NULL,
    status TEXT NOT NULL,
    paid_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY(import_id,row_number)
);

CREATE OR REPLACE FUNCTION reject_provider_bill_import_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'provider bill imports are immutable';
END $$;
CREATE TRIGGER provider_bill_imports_immutable BEFORE UPDATE OR DELETE ON provider_bill_imports
FOR EACH ROW EXECUTE FUNCTION reject_provider_bill_import_mutation();
CREATE TRIGGER provider_bill_import_rows_immutable BEFORE UPDATE OR DELETE ON provider_bill_import_rows
FOR EACH ROW EXECUTE FUNCTION reject_provider_bill_import_mutation();

CREATE TABLE payment_provider_controls (
    provider_id TEXT NOT NULL,
    provider_version TEXT NOT NULL,
    new_orders_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    reason TEXT,
    version BIGINT NOT NULL DEFAULT 1 CHECK (version>0),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY(provider_id,provider_version)
);

CREATE INDEX payment_orders_late_boundary_idx
    ON payment_orders(late_review_boundary_at,id)
    WHERE status IN ('CREATED','PENDING') AND archive_status='ACTIVE';

CREATE INDEX payment_orders_archive_eligible_idx
    ON payment_orders(final_archive_eligible_at,id)
    WHERE archive_status='ACTIVE';
