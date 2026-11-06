DROP TRIGGER manual_review_stop_loss_evaluations_immutable ON manual_review_stop_loss_evaluations;
DROP TRIGGER manual_review_assignments_immutable ON manual_review_assignments;
DROP FUNCTION reject_manual_review_sla_evidence_mutation();
DROP TABLE manual_review_stop_loss_evaluations;
DROP INDEX manual_review_assignments_case_idx;
DROP TABLE manual_review_assignments;
ALTER TABLE manual_review_cases
    DROP COLUMN stop_loss_evaluated_at,
    DROP COLUMN last_assigned_at,
    DROP COLUMN assignment_revision;
