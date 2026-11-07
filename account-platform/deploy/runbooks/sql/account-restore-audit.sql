\set ON_ERROR_STOP on
BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY;
SET LOCAL search_path = pg_catalog, public, pg_temp;
SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '5s';
SET LOCAL TIME ZONE 'UTC';

SELECT 1 / CASE WHEN current_database() = :'audit_database' THEN 1 ELSE 0 END AS audit_database_guard;

-- Queries below enforce column privileges. RLS must not hide audited rows.
SELECT 1 / CASE WHEN bool_and(NOT row_security_active(relation)) THEN 1 ELSE 0 END AS audit_visibility_guard
FROM unnest(ARRAY[
    'public.schema_migrations','public.users','public.organizations','public.memberships',
    'public.membership_units','public.organization_units','public.roles','public.role_assignments',
    'public.role_permissions','public.authorization_versions','public.wallet_provisioning_jobs'
]::regclass[]) AS required(relation);

SELECT 1 / CASE WHEN EXISTS (
    SELECT FROM public.schema_migrations WHERE version = 2 AND name = 'account_domain'
) THEN 1 ELSE 0 END AS audit_domain_guard;

SELECT 'account' AS audit_domain, transaction_timestamp() AS captured_at;

SELECT 'users' AS fact, status, count(*) AS total FROM public.users GROUP BY status
UNION ALL
SELECT 'organizations', status, count(*) FROM public.organizations GROUP BY status
UNION ALL
SELECT 'memberships', status, count(*) FROM public.memberships GROUP BY status
UNION ALL
SELECT 'wallet_provisioning_jobs', status, count(*) FROM public.wallet_provisioning_jobs GROUP BY status
ORDER BY fact, status;

SELECT 'role_assignments' AS fact, count(*) AS total FROM public.role_assignments
UNION ALL
SELECT 'role_permissions', count(*) FROM public.role_permissions
UNION ALL
SELECT 'authorization_versions', count(*) FROM public.authorization_versions
ORDER BY fact;

-- Check the role-assignment scope shape and cross-organization unit links.
SELECT 'role_assignment_scope_mismatch' AS check_name, count(*) AS violations
FROM public.role_assignments assignment
LEFT JOIN public.roles role ON role.code = assignment.role_code
LEFT JOIN public.organization_units unit ON unit.id = assignment.unit_id
WHERE role.code IS NULL
   OR (role.scope IN ('PERSONAL','PLATFORM') AND (assignment.organization_id IS NOT NULL OR assignment.unit_id IS NOT NULL))
   OR (role.scope = 'ORGANIZATION' AND (assignment.organization_id IS NULL OR assignment.unit_id IS NOT NULL))
   OR (role.scope = 'UNIT' AND (assignment.organization_id IS NULL OR assignment.unit_id IS NULL
       OR unit.organization_id IS DISTINCT FROM assignment.organization_id))
UNION ALL
SELECT 'membership_unit_organization_mismatch', count(*)
FROM public.membership_units assignment
JOIN public.memberships membership ON membership.id = assignment.membership_id
JOIN public.organization_units unit ON unit.id = assignment.unit_id
WHERE unit.organization_id <> membership.organization_id
ORDER BY check_name;

ROLLBACK;
