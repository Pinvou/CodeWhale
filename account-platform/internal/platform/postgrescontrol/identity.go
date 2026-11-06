// Package postgrescontrol contains PostgreSQL operations primitives. It never
// grants authority to an application identity or changes business facts.
package postgrescontrol

// TrustedAdministratorSQL uses the fixed catalog alias r. The pg_ namespace
// cannot be provisioned by ordinary roles. RDS authority requires both the
// platform membership and its actual control attributes; names alone do not
// establish authority. Callers must still reject other administrator sessions.
const TrustedAdministratorSQL = `(r.rolsuper OR (
 r.rolcreaterole AND r.rolcreatedb AND r.rolbypassrls AND EXISTS (
 SELECT 1 FROM pg_catalog.pg_roles platform
 WHERE platform.rolname='pg_rds_superuser' AND NOT platform.rolcanlogin
 AND pg_catalog.pg_has_role(r.oid,platform.oid,'MEMBER'))))`
