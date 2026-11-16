\set ON_ERROR_STOP on
BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY;
SET LOCAL search_path = pg_catalog, public, pg_temp;
SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '5s';
SET LOCAL TIME ZONE 'UTC';

SELECT 1 / CASE WHEN current_database() = :'audit_database' THEN 1 ELSE 0 END AS audit_database_guard;

SELECT 1 / CASE WHEN bool_and(NOT row_security_active(relation)) THEN 1 ELSE 0 END AS audit_visibility_guard
FROM unnest(ARRAY[
    'public.schema_migrations','public.usage_requests','public.usage_attempts','public.token_accounts'
]::regclass[]) AS required(relation);

SELECT 1 / CASE WHEN EXISTS (
    SELECT FROM public.schema_migrations WHERE version = 2 AND name = 'token_domain'
) THEN 1 ELSE 0 END AS audit_domain_guard;

SELECT 'token' AS audit_domain, transaction_timestamp() AS captured_at;

SELECT 'token_accounts' AS fact, status, count(*) AS total FROM public.token_accounts GROUP BY status
UNION ALL
SELECT 'usage_attempts', status, count(*) FROM public.usage_attempts GROUP BY status
ORDER BY fact, status;

SELECT status, settlement_status, count(*) AS total,
       count(actual_microcredits) AS requests_with_actual,
       COALESCE(sum(actual_microcredits), 0) AS actual_microcredits
FROM public.usage_requests
GROUP BY status, settlement_status
ORDER BY status, settlement_status;

-- A committed usage has a settled amount, including legitimate zero usage.
SELECT 'committed_usage_settlement_mismatch' AS check_name, count(*) AS violations
FROM public.usage_requests
WHERE (status = 'COMMITTED') <> (settlement_status = 'SETTLED')
   OR (status = 'COMMITTED' AND actual_microcredits IS NULL);

ROLLBACK;
