\set ON_ERROR_STOP on
BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY;
SET LOCAL search_path = pg_catalog, public, pg_temp;
SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '5s';
SET LOCAL TIME ZONE 'UTC';

SELECT 1 / CASE WHEN current_database() = :'audit_database' THEN 1 ELSE 0 END AS audit_database_guard;

SELECT 1 / CASE WHEN bool_and(NOT row_security_active(relation)) THEN 1 ELSE 0 END AS audit_visibility_guard
FROM unnest(ARRAY[
    'public.schema_migrations','public.outbox_events','public.inbox_messages','public.inbox_aggregate_versions'
]::regclass[]) AS required(relation);

SELECT 1 / CASE WHEN EXISTS (
    SELECT FROM public.schema_migrations
    WHERE version = 2 AND name = CASE :'audit_domain'
        WHEN 'account' THEN 'account_domain' WHEN 'billing' THEN 'ledger' WHEN 'token' THEN 'token_domain'
    END
) THEN 1 ELSE 0 END AS audit_domain_guard;

SELECT :'audit_domain' AS audit_domain, transaction_timestamp() AS captured_at;

-- Account's authorization events share the table with the ordinary outbox.
-- Inventory this partition separately; never read the frozen sidecar bytes.
WITH partitions AS (
    SELECT 'ordinary' AS partition
    UNION ALL SELECT 'authorization' WHERE :'audit_domain' = 'account'
), deliveries AS (
    SELECT CASE WHEN :'audit_domain' = 'account' AND event_type = 'account.approval-command.authorized.v1'
                THEN 'authorization' ELSE 'ordinary' END AS partition,
           published_at, dead_lettered_at, locked_by, locked_until
    FROM public.outbox_events
)
SELECT partitions.partition, count(deliveries.partition) AS total,
       count(*) FILTER (WHERE published_at IS NOT NULL) AS published,
       count(*) FILTER (WHERE dead_lettered_at IS NOT NULL) AS dead_lettered,
       count(deliveries.partition) FILTER (WHERE published_at IS NULL AND dead_lettered_at IS NULL) AS pending,
       count(*) FILTER (WHERE published_at IS NULL AND dead_lettered_at IS NULL
                          AND locked_until > transaction_timestamp()) AS active_leases,
       count(*) FILTER (WHERE published_at IS NULL AND dead_lettered_at IS NULL
                          AND locked_until <= transaction_timestamp()) AS expired_leases,
       count(*) FILTER (WHERE (published_at IS NOT NULL AND dead_lettered_at IS NOT NULL)
                           OR ((locked_by IS NULL) <> (locked_until IS NULL))
                           OR ((published_at IS NOT NULL OR dead_lettered_at IS NOT NULL)
                               AND locked_until IS NOT NULL)) AS inconsistent_rows
FROM partitions LEFT JOIN deliveries USING (partition)
GROUP BY partitions.partition
ORDER BY partitions.partition;

SELECT count(*) AS total, count(processed_at) AS processed,
       count(*) FILTER (WHERE processed_at IS NULL) AS pending
FROM public.inbox_messages;

SELECT count(*) AS aggregate_versions FROM public.inbox_aggregate_versions;

ROLLBACK;
