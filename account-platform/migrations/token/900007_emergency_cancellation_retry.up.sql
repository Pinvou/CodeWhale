ALTER TABLE model_emergency_cancellations
    DROP CONSTRAINT model_emergency_cancellations_status_check,
    ADD CONSTRAINT model_emergency_cancellations_status_check
        CHECK (status IN ('PENDING','LEASED','CONVERGING','CONVERGED','MANUAL_REVIEW')),
    ADD COLUMN retry_attempts INTEGER NOT NULL DEFAULT 0 CHECK (retry_attempts>=0),
    ADD COLUMN next_attempt_at TIMESTAMPTZ;

ALTER TABLE model_emergency_cancellation_evidence
    DROP CONSTRAINT model_emergency_cancellation_evidence_evidence_type_check,
    ADD CONSTRAINT model_emergency_cancellation_evidence_evidence_type_check
        CHECK (evidence_type IN ('REQUESTED','ATTEMPTED','CONVERGED','MANUAL_REVIEW'));
