DROP TRIGGER IF EXISTS usage_execution_transitions_immutable ON usage_execution_transitions;
DROP FUNCTION IF EXISTS reject_usage_execution_history_mutation();
DROP TABLE IF EXISTS usage_replay_case_payloads;
DROP TABLE IF EXISTS usage_replay_retention_cases;
DROP TABLE IF EXISTS usage_idempotency_tombstones;
DROP TABLE IF EXISTS usage_replay_results;
DROP TABLE IF EXISTS usage_stream_events;
DROP TABLE IF EXISTS usage_dispatch_preparations;
DROP TABLE IF EXISTS usage_dispatch_intents;
DROP TABLE IF EXISTS usage_execution_transitions;
DROP INDEX IF EXISTS usage_requests_inference_idempotency_idx;

ALTER TABLE usage_requests ADD COLUMN idempotency_key TEXT;
UPDATE usage_requests SET idempotency_key=COALESCE(idempotency_key_digest,'expired-'||id::text);
ALTER TABLE usage_requests ALTER COLUMN idempotency_key SET NOT NULL;
ALTER TABLE usage_requests ADD CONSTRAINT usage_requests_idempotency_key_key UNIQUE(idempotency_key);

ALTER TABLE usage_requests
    DROP COLUMN dispatch_lease_until,
    DROP COLUMN dispatch_lease_owner,
    DROP COLUMN delivery_status,
    DROP COLUMN execution_version,
    DROP COLUMN execution_status,
    DROP COLUMN request_fingerprint,
    DROP COLUMN canonicalization_version,
    DROP COLUMN idempotency_key_digest,
    DROP COLUMN idempotency_namespace;
