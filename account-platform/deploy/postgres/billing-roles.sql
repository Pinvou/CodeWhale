\if :{?billing_database}
\else
\echo 'billing_database must explicitly name the dedicated target database'
\quit 1
\endif
SELECT current_database() = :'billing_database' AS billing_target_matches \gset
\if :billing_target_matches
\else
\echo 'connected database differs from the explicit Billing target'
\quit 1
\endif
BEGIN;
-- Run only as a database role administrator, against the dedicated Billing
-- database. Authentication/passwords are supplied by operator secret delivery.
-- This script never changes existing credentials or transfers unrelated objects.
DO $$ BEGIN
 IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='billing_service') THEN
  CREATE ROLE billing_service LOGIN INHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
 END IF;
 IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='billing_schema_owner') THEN
  CREATE ROLE billing_schema_owner NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
 END IF;
 IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='billing_migrator') THEN
  CREATE ROLE billing_migrator LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
 END IF;
 IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname IN ('billing_service','billing_migrator','billing_schema_owner')
 AND (rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls))
 OR EXISTS(SELECT 1 FROM pg_roles WHERE rolname='billing_schema_owner' AND rolcanlogin)
 OR EXISTS(SELECT 1 FROM pg_roles WHERE rolname='billing_migrator' AND (NOT rolcanlogin OR rolinherit))
 OR EXISTS(SELECT 1 FROM pg_roles WHERE rolname='billing_service' AND NOT rolcanlogin)
 OR pg_has_role('billing_service','billing_schema_owner','MEMBER') THEN
  RAISE EXCEPTION 'unsafe existing billing identities require operator investigation';
 END IF;
END $$;
-- One-way inheritance lets the trusted owner reclaim legacy runtime-owned
-- objects. Runtime gains no membership or SET authority in the owner.
GRANT billing_service TO billing_schema_owner WITH INHERIT TRUE,SET TRUE;
GRANT billing_service,billing_schema_owner TO billing_migrator WITH INHERIT FALSE,SET TRUE;
DO $$ BEGIN
 IF to_regclass('public.billing_runtime_hardening') IS NULL THEN
  ALTER SCHEMA public OWNER TO billing_schema_owner;
  REVOKE CREATE ON SCHEMA public FROM PUBLIC;
  GRANT USAGE,CREATE ON SCHEMA public TO billing_service;
 END IF;
END $$;

COMMIT;
