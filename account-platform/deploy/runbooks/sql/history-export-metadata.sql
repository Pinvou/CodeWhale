\set ON_ERROR_STOP on
BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY;
SET LOCAL search_path = pg_catalog, public, pg_temp;
SET LOCAL statement_timeout = '30s';
SET LOCAL lock_timeout = '5s';
SET LOCAL TIME ZONE 'UTC';
SELECT 1 / CASE WHEN :'audit_domain' IN ('account','billing','token') AND EXISTS (
 SELECT FROM public.schema_migrations WHERE version=2 AND name=CASE :'audit_domain'
 WHEN 'account' THEN 'account_domain' WHEN 'billing' THEN 'ledger' ELSE 'token_domain' END
) THEN 1 ELSE 0 END AS domain_guard;
SELECT 1 / CASE WHEN bool_and(NOT row_security_active(relation::regclass) AND has_table_privilege(current_user,relation,'SELECT'))
 THEN 1 ELSE 0 END AS visibility_guard
FROM unnest(CASE WHEN :'audit_domain'='billing' THEN
 ARRAY['public.schema_migrations','public.outbox_events','public.payment_provider_health_observations']::text[]
 ELSE ARRAY['public.schema_migrations','public.outbox_events']::text[] END) required(relation);
SELECT 1 / CASE WHEN (SELECT count(*) FROM (SELECT version FROM public.schema_migrations LIMIT 2049) bounded)<=2048
 THEN 1 ELSE 0 END AS ledger_size_guard;
SELECT json_build_object('domain',:'audit_domain','database',current_database(),
 'database_oid',(SELECT oid FROM pg_database WHERE datname=current_database()),
 'database_role',current_user,'server_version',current_setting('server_version_num'),
 'postmaster_started_at',pg_postmaster_start_time(),'observed_at',clock_timestamp(),
 'storage_inventory',(SELECT json_agg(json_build_object('relation',c.relname,
 'total_bytes',pg_total_relation_size(c.oid),'estimated_rows',c.reltuples,
 'rows_are_estimates',true) ORDER BY c.relname)
 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname='public' AND c.relname=ANY(CASE WHEN :'audit_domain'='billing' THEN
 ARRAY['schema_migrations','outbox_events','payment_provider_health_observations']
 ELSE ARRAY['schema_migrations','outbox_events'] END)),
 'migrations',(SELECT json_agg(json_build_object('version',version,'name',name,'checksum',checksum) ORDER BY version)
 FROM public.schema_migrations))::text;
ROLLBACK;
