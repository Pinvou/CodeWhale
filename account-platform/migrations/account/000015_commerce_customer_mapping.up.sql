CREATE TABLE commerce_customer_mappings (
    id UUID PRIMARY KEY,
    subject_type TEXT NOT NULL CHECK (subject_type IN ('USER', 'ORGANIZATION')),
    subject_id UUID NOT NULL,
    provider_id TEXT NOT NULL,
    adapter_version TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('PENDING', 'IN_PROGRESS', 'READY', 'RETRY', 'REPAIR_REQUIRED', 'DETACHED')),
    external_reference_redacted TEXT,
    version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
    last_error_code TEXT,
    provisioned_at TIMESTAMPTZ,
    detached_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE (subject_type, subject_id, provider_id)
);

CREATE TABLE commerce_customer_sync_jobs (
    id UUID PRIMARY KEY,
    mapping_id UUID NOT NULL REFERENCES commerce_customer_mappings(id),
    mapping_version BIGINT NOT NULL CHECK (mapping_version > 0),
    subject_type TEXT NOT NULL CHECK (subject_type IN ('USER', 'ORGANIZATION')),
    subject_id UUID NOT NULL,
    subject_lifecycle_version BIGINT NOT NULL DEFAULT 1 CHECK (subject_lifecycle_version > 0),
    provider_id TEXT NOT NULL,
    adapter_version TEXT NOT NULL,
    task_category TEXT NOT NULL CHECK (task_category IN ('CUSTOMER_PROVISIONING', 'PROFILE_SYNC', 'FINANCIAL')),
    related_business_reference TEXT,
    contact_mobile_e164 TEXT,
    status TEXT NOT NULL CHECK (status IN ('PENDING', 'LEASED', 'RETRY', 'COMPLETED', 'REPAIR_REQUIRED', 'CANCELLED_ACCOUNT_CLOSURE')),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL,
    lease_until TIMESTAMPTZ,
    last_error_code TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ
);

CREATE INDEX commerce_customer_sync_jobs_active_mapping_idx
    ON commerce_customer_sync_jobs(mapping_id)
    WHERE status IN ('PENDING', 'LEASED', 'RETRY');
CREATE INDEX commerce_customer_sync_jobs_ready_idx
    ON commerce_customer_sync_jobs(next_attempt_at, created_at)
    WHERE status IN ('PENDING', 'RETRY');

CREATE TABLE commerce_customer_sync_evidence (
    id UUID PRIMARY KEY,
    job_id UUID NOT NULL REFERENCES commerce_customer_sync_jobs(id),
    mapping_id UUID NOT NULL REFERENCES commerce_customer_mappings(id),
    provider_id TEXT NOT NULL,
    adapter_version TEXT NOT NULL,
    outcome TEXT NOT NULL CHECK (outcome IN ('SUCCEEDED', 'FAILED', 'STALE')),
    external_reference_redacted TEXT,
    error_code TEXT,
    recorded_at TIMESTAMPTZ NOT NULL
);

INSERT INTO commerce_customer_mappings(
    id, subject_type, subject_id, provider_id, adapter_version, status,
    external_reference_redacted, last_error_code, provisioned_at, created_at, updated_at
)
SELECT user_id, 'USER', user_id, 'h3c_mall', 'h3c-mall-v1',
       CASE status WHEN 'SYNCED' THEN 'READY' WHEN 'LEASED' THEN 'RETRY' ELSE status END,
       CASE WHEN buyer_id IS NULL OR buyer_id = '' THEN NULL ELSE '***' || right(buyer_id, 4) END,
       CASE WHEN last_error IS NULL THEN NULL ELSE 'COMMERCE_PROVIDER_UNAVAILABLE' END,
       synced_at, created_at, updated_at
FROM mall_user_sync_jobs;

INSERT INTO commerce_customer_sync_jobs(
    id, mapping_id, mapping_version, subject_type, subject_id, provider_id, adapter_version,
    task_category, contact_mobile_e164, status, attempts, next_attempt_at,
    lease_until, last_error_code, created_at, updated_at, completed_at
)
SELECT user_id, user_id, 1, 'USER', user_id, 'h3c_mall', 'h3c-mall-v1',
       'CUSTOMER_PROVISIONING', mobile_e164,
       CASE status WHEN 'SYNCED' THEN 'COMPLETED' WHEN 'LEASED' THEN 'RETRY' ELSE status END,
       attempts, next_attempt_at, NULL,
       CASE WHEN last_error IS NULL THEN NULL ELSE 'COMMERCE_PROVIDER_UNAVAILABLE' END,
       created_at, updated_at, synced_at
FROM mall_user_sync_jobs;

DROP TABLE mall_user_sync_jobs;
