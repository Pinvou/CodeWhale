\set ON_ERROR_STOP on
BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY;
SET LOCAL search_path = pg_catalog, pg_temp;
SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '5s';

-- Read the ledger before resolving the approval relation. Histories before
-- migration 018 legitimately lack that relation.
SELECT 1 / CASE WHEN current_database()::text = :'expected_database'
    AND has_column_privilege(current_user, 'public.schema_migrations', 'version', 'SELECT')
    AND has_column_privilege(current_user, 'public.schema_migrations', 'name', 'SELECT')
    AND has_column_privilege(current_user, 'public.schema_migrations', 'checksum', 'SELECT')
    AND NOT row_security_active('public.schema_migrations'::regclass)
    THEN 1 ELSE 0 END AS account_039_inventory_guard;

SELECT COALESCE(max(version), 0) >= 18 AS has_approval_history
FROM public.schema_migrations \gset

\if :has_approval_history
-- Once 018 is expected, missing data, denied columns and active RLS must
-- fail the probe rather than produce a false zero.
SELECT 1 / CASE WHEN has_column_privilege(current_user, 'public.approval_requests', 'command_type', 'SELECT')
    AND has_column_privilege(current_user, 'public.approval_requests', 'policy_version', 'SELECT')
    AND NOT row_security_active('public.approval_requests'::regclass)
    THEN 1 ELSE 0 END AS account_039_approval_visibility_guard
\gset

-- Match the precise policy keys deleted by the frozen 039 Down. All request
-- statuses retain the original foreign key, even terminal ones.
SELECT count(*) AS v2_policy_request_references
FROM public.approval_requests
WHERE policy_version = 2 AND command_type IN (
    'ACCOUNT_ORGANIZATION_RENAME',
    'ACCOUNT_MEMBER_ADD', 'ACCOUNT_MEMBER_ROLES_REPLACE', 'ACCOUNT_MEMBER_REMOVE',
    'ACCOUNT_UNIT_CREATE', 'ACCOUNT_UNIT_RENAME', 'ACCOUNT_UNIT_DELETE'
) \gset
\else
\set v2_policy_request_references 0
\endif

WITH ledger AS (
    SELECT max(version) AS highest_version FROM public.schema_migrations
), installed AS (
    SELECT version, name, checksum,
        name = 'organization_governance_permissions' AND checksum IN (
            -- Frozen 039 Up, in its supported LF and historical CRLF forms.
            '47aa9cccf678caaba684ee9d6d2cc133bb98b8f4d20389ded3037e28c6f330e7',
            '03cce57bcb49990e2eaa3a775e22d049e2edde7ffe7f9485a3492eb635bc9af3'
        ) AS identity_matches
    FROM public.schema_migrations WHERE version = 39
)
SELECT transaction_timestamp() AT TIME ZONE 'UTC' AS checked_at_utc,
    ledger.highest_version,
    installed.name AS installed_039_name,
    installed.checksum AS installed_039_checksum,
    installed.identity_matches AS installed_039_identity_matches,
    :'v2_policy_request_references'::bigint AS v2_policy_request_references,
    CASE
        WHEN ledger.highest_version IS NULL THEN 'EMPTY_MIGRATION_LEDGER'
        WHEN installed.version IS NULL AND ledger.highest_version > 39 THEN 'MISSING_039_ON_LATER_LEDGER'
        WHEN installed.version IS NULL THEN '039_NOT_INSTALLED'
        WHEN installed.identity_matches IS NOT TRUE THEN '039_IDENTITY_MISMATCH'
        WHEN ledger.highest_version > 39 THEN 'LATER_MIGRATIONS_PRESENT'
        WHEN :'v2_policy_request_references'::bigint > 0 THEN '039_V2_POLICY_REFERENCED'
        ELSE '039_RESTORE_REHEARSAL_REQUIRED'
    END AS assessment
FROM ledger LEFT JOIN installed ON true;

ROLLBACK;
