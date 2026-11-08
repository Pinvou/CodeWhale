-- Reuse immutable PROVIDER_BLIND evidence. Old rows carry no versioned command
-- receipt and are neither rewritten nor inferred to prove an unknown key.
CREATE UNIQUE INDEX late_payment_provider_blind_command_key
ON late_payment_review_evidence (
    actor_id,
    (payload_snapshot->'provider_blind_command'->>'idempotency_key')
)
WHERE evidence_type='PROVIDER_BLIND'
  AND payload_snapshot->'provider_blind_command'->>'version'='1';
