DROP INDEX usage_attempts_upstream_reference_idx;
ALTER TABLE usage_attempts DROP CONSTRAINT usage_attempts_upstream_evidence_complete;
ALTER TABLE usage_attempts ADD COLUMN newapi_request_id TEXT;
UPDATE usage_attempts SET newapi_request_id=upstream_request_id WHERE provider_id IN ('NEW-API','NEW_API');
ALTER TABLE usage_attempts
    DROP COLUMN upstream_reference_digest,
    DROP COLUMN adapter_version,
    DROP COLUMN provider_id;
ALTER TABLE usage_attempts RENAME COLUMN upstream_request_id TO provider_request_id;
ALTER TABLE routing_policies DROP COLUMN protocol_code;
