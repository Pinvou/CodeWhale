ALTER TABLE manual_adjustments
    ADD COLUMN version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
    ADD COLUMN request_idempotency_key TEXT,
    ADD COLUMN approval_request_id UUID,
    ADD COLUMN execution_idempotency_key TEXT,
    ADD CONSTRAINT manual_adjustments_approval_request_unique UNIQUE (approval_request_id),
    ADD CONSTRAINT manual_adjustments_execution_key_unique UNIQUE (execution_idempotency_key),
    ADD CONSTRAINT manual_adjustments_approval_execution_pair CHECK (
        (approval_request_id IS NULL) = (execution_idempotency_key IS NULL)
    );

CREATE UNIQUE INDEX manual_adjustments_request_idempotency_unique
ON manual_adjustments(requested_by,request_idempotency_key)
WHERE request_idempotency_key IS NOT NULL;

CREATE FUNCTION require_adjustment_approval_execution() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status = 'POSTED' AND (NEW.approval_request_id IS NULL OR NEW.execution_idempotency_key IS NULL) THEN
        IF TG_OP='INSERT' THEN
            RAISE EXCEPTION 'new financial adjustments require an approved execution' USING ERRCODE='23514';
        ELSIF OLD.status <> 'POSTED' THEN
            RAISE EXCEPTION 'new financial adjustments require an approved execution' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER manual_adjustments_require_approval_execution
BEFORE INSERT OR UPDATE ON manual_adjustments
FOR EACH ROW EXECUTE FUNCTION require_adjustment_approval_execution();
