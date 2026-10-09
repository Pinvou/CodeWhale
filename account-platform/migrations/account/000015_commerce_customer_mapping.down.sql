CREATE TABLE mall_user_sync_jobs (
    user_id UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    mobile_e164 TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('PENDING', 'RETRY', 'LEASED', 'SYNCED')),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL,
    lease_until TIMESTAMPTZ,
    last_error TEXT,
    buyer_id TEXT,
    synced_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

INSERT INTO mall_user_sync_jobs(
    user_id, mobile_e164, status, attempts, next_attempt_at, lease_until,
    last_error, synced_at, created_at, updated_at
)
SELECT m.subject_id, j.contact_mobile_e164,
       CASE m.status WHEN 'READY' THEN 'SYNCED' WHEN 'IN_PROGRESS' THEN 'RETRY' WHEN 'REPAIR_REQUIRED' THEN 'RETRY' ELSE m.status END,
       j.attempts, j.next_attempt_at, NULL, j.last_error_code,
       m.provisioned_at, m.created_at, m.updated_at
FROM commerce_customer_mappings m
JOIN LATERAL (
    SELECT * FROM commerce_customer_sync_jobs candidate
    WHERE candidate.mapping_id = m.id AND candidate.contact_mobile_e164 IS NOT NULL
    ORDER BY candidate.created_at DESC LIMIT 1
) j ON true
WHERE m.subject_type='USER' AND m.status <> 'DETACHED';

CREATE INDEX mall_user_sync_jobs_ready_idx
    ON mall_user_sync_jobs (next_attempt_at, updated_at)
    WHERE status IN ('PENDING', 'RETRY');

DROP TABLE commerce_customer_sync_evidence;
DROP TABLE commerce_customer_sync_jobs;
DROP TABLE commerce_customer_mappings;
