CREATE INDEX usage_requests_operations_overview_started_idx
    ON usage_requests(started_at, id);

CREATE INDEX manual_review_cases_token_usage_overview_created_idx
    ON manual_review_cases(created_at, id)
    INCLUDE (resource_id, status)
    WHERE domain = 'TOKEN'
      AND case_type = 'USAGE_RECONCILIATION_REVIEW'
      AND resource_type = 'USAGE_REQUEST';
