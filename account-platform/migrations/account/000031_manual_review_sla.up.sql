CREATE TABLE manual_review_cases (
    id UUID PRIMARY KEY,
    domain TEXT NOT NULL CHECK (domain='ACCOUNT'),
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
    assignment_revision BIGINT NOT NULL DEFAULT 0 CHECK (assignment_revision>=0),
    last_assigned_at TIMESTAMPTZ,
    stop_loss_evaluated_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE(domain,case_type,resource_type,resource_id)
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
CREATE INDEX manual_review_case_events_case_idx ON manual_review_case_events(case_id,created_at,id);

CREATE TABLE manual_review_assignments (
    id UUID PRIMARY KEY,
    case_id UUID NOT NULL REFERENCES manual_review_cases(id),
    from_owner_id UUID,
    to_owner_id UUID NOT NULL,
    escalation_level INTEGER NOT NULL CHECK (escalation_level>=0),
    reason TEXT NOT NULL,
    assigned_at TIMESTAMPTZ NOT NULL,
    UNIQUE(case_id,escalation_level)
);

CREATE TABLE manual_review_stop_loss_evaluations (
    id UUID PRIMARY KEY,
    case_id UUID NOT NULL UNIQUE REFERENCES manual_review_cases(id),
    severity TEXT NOT NULL CHECK (severity IN ('P0','P1')),
    high_risk_command_type TEXT,
    action_snapshot JSONB NOT NULL,
    evaluated_at TIMESTAMPTZ NOT NULL
);

CREATE FUNCTION reject_account_manual_review_evidence_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'manual review evidence is immutable';
END $$;
CREATE TRIGGER manual_review_case_events_immutable BEFORE UPDATE OR DELETE ON manual_review_case_events
FOR EACH ROW EXECUTE FUNCTION reject_account_manual_review_evidence_mutation();
CREATE TRIGGER manual_review_assignments_immutable BEFORE UPDATE OR DELETE ON manual_review_assignments
FOR EACH ROW EXECUTE FUNCTION reject_account_manual_review_evidence_mutation();
CREATE TRIGGER manual_review_stop_loss_evaluations_immutable BEFORE UPDATE OR DELETE ON manual_review_stop_loss_evaluations
FOR EACH ROW EXECUTE FUNCTION reject_account_manual_review_evidence_mutation();
