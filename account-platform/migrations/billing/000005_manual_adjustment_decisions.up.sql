ALTER TABLE manual_adjustments
    ADD COLUMN rejected_by UUID,
    ADD COLUMN decision_reason TEXT,
    ADD COLUMN decided_at TIMESTAMPTZ,
    ADD CONSTRAINT manual_adjustments_single_decision CHECK (approved_by IS NULL OR rejected_by IS NULL),
    ADD CONSTRAINT manual_adjustments_rejector_separation CHECK (rejected_by IS NULL OR rejected_by <> requested_by),
    ADD CONSTRAINT manual_adjustments_rejection_audit CHECK (
        status <> 'REJECTED'
        OR (rejected_by IS NOT NULL AND decision_reason IS NOT NULL AND btrim(decision_reason) <> '' AND decided_at IS NOT NULL)
    ),
    ADD CONSTRAINT manual_adjustments_approval_audit CHECK (
        status NOT IN ('APPROVED', 'POSTED')
        OR (approved_by IS NOT NULL AND decision_reason IS NOT NULL AND btrim(decision_reason) <> '' AND decided_at IS NOT NULL)
    ),
    ADD CONSTRAINT manual_adjustments_pending_undecided CHECK (
        status <> 'PENDING_APPROVAL'
        OR (approved_by IS NULL AND rejected_by IS NULL AND decision_reason IS NULL AND decided_at IS NULL)
    );
