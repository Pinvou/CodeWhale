-- Expired leases remain reclaimable independently of the ready-job index.
CREATE INDEX commerce_customer_sync_jobs_leased_idx
    ON commerce_customer_sync_jobs(provider_id,adapter_version,lease_until,next_attempt_at,created_at)
    WHERE status='LEASED';
