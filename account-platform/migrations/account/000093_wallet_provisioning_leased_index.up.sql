CREATE INDEX wallet_provisioning_leased_idx
    ON wallet_provisioning_jobs (lease_until)
    WHERE status = 'LEASED';
