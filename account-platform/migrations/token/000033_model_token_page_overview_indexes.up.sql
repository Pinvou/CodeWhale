CREATE INDEX routing_policies_overview_health_idx
    ON routing_policies(health_status, health_expires_at, id)
    WHERE reference_status = 'RESOLVED';
