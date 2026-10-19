package tokenschema

import (
	"context"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
)

func prepareTokenMigration(ctx context.Context, tx pgx.Tx, m migrations.Migration) error {
	var actor bool
	if err := tx.QueryRow(ctx, `SELECT session_user='token_migrator' AND current_user='token_gateway'
 AND pg_has_role(session_user,'token_schema_owner','MEMBER') AND pg_has_role(session_user,'token_cap_policy_deployer','MEMBER')
 AND pg_has_role('token_schema_owner','token_gateway','USAGE')
 AND NOT pg_has_role('token_gateway','token_schema_owner','MEMBER') AND NOT pg_has_role('token_gateway','token_cap_policy_deployer','MEMBER')
 AND NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname IN ('token_gateway','token_schema_owner','token_cap_policy_deployer','token_migrator','token_policy_operator') AND (rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls))
 AND NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname IN ('token_schema_owner','token_cap_policy_deployer') AND rolcanlogin)
 AND NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname IN ('token_migrator','token_policy_operator') AND (rolinherit OR NOT rolcanlogin))
 AND NOT EXISTS(SELECT 1 FROM pg_roles WHERE (rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls) AND pg_has_role('token_gateway',oid,'MEMBER'))`).Scan(&actor); err != nil {
		return err
	}
	if !actor {
		return errors.New("Token migration requires isolated operator-provisioned roles")
	}
	var hardened bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('public.token_runtime_hardening') IS NOT NULL`).Scan(&hardened); err != nil {
		return err
	}
	if err := bridgeHistoryDefiners(ctx, tx, false); err != nil {
		return err
	}
	if !hardened {
		return lendPinnedMigrationDDL(ctx, tx, m)
	}
	// Authority is uncommitted and scoped to this single migration transaction.
	// Existing runtime connections see the prior owner/ACL until hardening commits.
	_, err := tx.Exec(ctx, `SET ROLE token_schema_owner;
 GRANT CREATE ON SCHEMA public TO token_gateway;
 DO $$ DECLARE object RECORD; BEGIN
 FOR object IN SELECT c.relname,c.relkind FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname='public' AND pg_get_userbyid(c.relowner)='token_schema_owner' AND c.relkind IN ('r','p','S','v','m')
 AND c.relname NOT IN ('schema_migrations','token_runtime_hardening','token_command_governance_activation','token_command_governance_transitions') ORDER BY (c.relkind='S'),c.oid LOOP
 EXECUTE format('ALTER %s public.%I OWNER TO token_gateway',CASE object.relkind WHEN 'S' THEN 'SEQUENCE' WHEN 'v' THEN 'VIEW' WHEN 'm' THEN 'MATERIALIZED VIEW' ELSE 'TABLE' END,object.relname);
 END LOOP;
 FOR object IN SELECT p.oid::regprocedure::text AS signature FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
 WHERE n.nspname='public' AND pg_get_userbyid(p.proowner)='token_schema_owner'
 AND p.proname NOT IN ('harden_token_runtime_objects','reject_token_truncate','reject_token_audit_mutation','project_usage_history_request','maintain_usage_history_projection','backfill_usage_history_projection') LOOP
 EXECUTE format('ALTER FUNCTION %s OWNER TO token_gateway',object.signature);
 END LOOP;
 END $$; SET ROLE token_gateway;`)
	return err
}
func finishTokenMigration(ctx context.Context, tx pgx.Tx, m migrations.Migration) error {
	var hardened bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('public.token_runtime_hardening') IS NOT NULL`).Scan(&hardened); err != nil {
		return err
	}
	if !hardened {
		return revokePinnedMigrationDDL(ctx, tx, m)
	}
	if err := bridgeHistoryDefiners(ctx, tx, false); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SET ROLE token_schema_owner; SELECT public.harden_token_runtime_objects();
 SET ROLE token_cap_policy_deployer;
 DO $$ DECLARE object RECORD; BEGIN
 FOR object IN SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname='public' AND c.relkind IN ('r','p') AND pg_get_userbyid(c.relowner)='token_cap_policy_deployer' LOOP
 EXECUTE format('DROP TRIGGER IF EXISTS token_no_truncate ON public.%I',object.relname);
 EXECUTE format('CREATE TRIGGER token_no_truncate BEFORE TRUNCATE ON public.%I FOR EACH STATEMENT EXECUTE FUNCTION public.reject_token_truncate()',object.relname);
 END LOOP;
 END $$; SET ROLE token_gateway;`)
	if err != nil {
		return err
	}
	return bridgeHistoryDefiners(ctx, tx, true)
}
