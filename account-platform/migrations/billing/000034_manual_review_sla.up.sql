ALTER TABLE manual_review_cases
    ADD COLUMN assignment_revision BIGINT NOT NULL DEFAULT 0 CHECK (assignment_revision>=0),
    ADD COLUMN last_assigned_at TIMESTAMPTZ,
    ADD COLUMN stop_loss_evaluated_at TIMESTAMPTZ;

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
CREATE INDEX manual_review_assignments_case_idx
    ON manual_review_assignments(case_id,assigned_at,id);

CREATE TABLE manual_review_stop_loss_evaluations (
    id UUID PRIMARY KEY,
    case_id UUID NOT NULL REFERENCES manual_review_cases(id),
    severity TEXT NOT NULL CHECK (severity IN ('P0','P1')),
    provider_id TEXT,
    provider_version TEXT,
    payment_product_id TEXT,
    route_reference TEXT,
    high_risk_command_type TEXT,
    action_snapshot JSONB NOT NULL,
    evaluated_at TIMESTAMPTZ NOT NULL,
    UNIQUE(case_id)
);

CREATE OR REPLACE FUNCTION reject_manual_review_sla_evidence_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'manual review assignment and stop-loss evidence is immutable';
END $$;
CREATE TRIGGER manual_review_assignments_immutable BEFORE UPDATE OR DELETE ON manual_review_assignments
FOR EACH ROW EXECUTE FUNCTION reject_manual_review_sla_evidence_mutation();
CREATE TRIGGER manual_review_stop_loss_evaluations_immutable BEFORE UPDATE OR DELETE ON manual_review_stop_loss_evaluations
FOR EACH ROW EXECUTE FUNCTION reject_manual_review_sla_evidence_mutation();
