CREATE EXTENSION IF NOT EXISTS pgcrypto;

ALTER TABLE routing_policies
    ADD COLUMN protocol_code TEXT NOT NULL DEFAULT 'CHAT';
ALTER TABLE routing_policies ALTER COLUMN protocol_code DROP DEFAULT;

ALTER TABLE usage_attempts
    RENAME COLUMN provider_request_id TO upstream_request_id;

ALTER TABLE usage_attempts
    ADD COLUMN provider_id TEXT,
    ADD COLUMN adapter_version TEXT,
    ADD COLUMN upstream_reference_digest TEXT
        CHECK (upstream_reference_digest IS NULL OR upstream_reference_digest ~ '^[0-9a-f]{64}$');

UPDATE usage_attempts
SET upstream_request_id=COALESCE(upstream_request_id,newapi_request_id),
    provider_id=CASE WHEN COALESCE(upstream_request_id,newapi_request_id) IS NULL THEN NULL ELSE 'NEW-API' END,
    adapter_version=CASE WHEN COALESCE(upstream_request_id,newapi_request_id) IS NULL THEN NULL ELSE 'new-api-v1' END,
    upstream_reference_digest=CASE
        WHEN COALESCE(upstream_request_id,newapi_request_id) IS NULL THEN NULL
        ELSE encode(digest(COALESCE(upstream_request_id,newapi_request_id),'sha256'),'hex')
    END;

ALTER TABLE usage_attempts DROP COLUMN newapi_request_id;

ALTER TABLE usage_attempts
    ADD CONSTRAINT usage_attempts_upstream_evidence_complete CHECK (
        (upstream_request_id IS NULL AND provider_id IS NULL AND adapter_version IS NULL AND upstream_reference_digest IS NULL)
        OR
        (upstream_request_id IS NOT NULL AND provider_id IS NOT NULL AND adapter_version IS NOT NULL AND upstream_reference_digest IS NOT NULL)
    );

CREATE INDEX usage_attempts_upstream_reference_idx
    ON usage_attempts(provider_id,adapter_version,upstream_reference_digest)
    WHERE upstream_request_id IS NOT NULL;
