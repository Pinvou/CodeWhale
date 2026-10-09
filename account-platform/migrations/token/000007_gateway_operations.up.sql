ALTER TABLE provider_credentials
    ADD COLUMN validation_status TEXT NOT NULL DEFAULT 'UNKNOWN'
        CHECK (validation_status IN ('UNKNOWN', 'VALID', 'INVALID')),
    ADD COLUMN validation_checked_by UUID;

UPDATE provider_credentials
SET validation_status = 'VALID'
WHERE validated_at IS NOT NULL;

CREATE TABLE token_audit_logs (
    id UUID PRIMARY KEY,
    actor_user_id UUID NOT NULL,
    action TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    resource_id UUID NOT NULL,
    summary JSONB NOT NULL DEFAULT '{}'::jsonb,
    occurred_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX token_audit_logs_occurred_idx
    ON token_audit_logs (occurred_at DESC, id DESC);
