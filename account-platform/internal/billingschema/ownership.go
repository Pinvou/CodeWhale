package billingschema

import (
	"context"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
	"strings"
)

// Original immutable 029 DDL excludes a former terminal state. Preserve old
// values byte-for-byte while applying its new-row check as NOT VALID.
const legacyAnomalyChecksum = "849e4ae4ff57132486992216005fc56a3f9a06c31a3cc84bc14c60c5fbe9bedb"
const chargebackStatusCheck = "ADD CONSTRAINT chargeback_cases_status_check CHECK (status IN ('REVIEW_REQUIRED','CONFIRMED','DISMISSED'))"

func applyBillingMigration(ctx context.Context, tx pgx.Tx, migration migrations.Migration) error {
	sql := migration.UpSQL
	if migration.Version == 29 {
		if sqlChecksum(sql) != legacyAnomalyChecksum || strings.Count(sql, chargebackStatusCheck) != 1 {
			return errors.New("billing 029 compatibility requires the exact retained migration")
		}
		// Prevent any old writer from racing the conditional representation.
		if _, err := tx.Exec(ctx, `LOCK TABLE chargeback_cases IN ACCESS EXCLUSIVE MODE`); err != nil {
			return err
		}
		var legacy bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM chargeback_cases WHERE status='RESOLVED')`).Scan(&legacy); err != nil {
			return err
		}
		if legacy {
			sql = strings.Replace(sql, chargebackStatusCheck, chargebackStatusCheck+" NOT VALID", 1)
		}
	}
	_, err := tx.Exec(ctx, sql)
	return err
}

func prepareBillingMigration(ctx context.Context, tx pgx.Tx, migration migrations.Migration) error {
	var actor bool
	if err := tx.QueryRow(ctx, `SELECT (session_user='billing_migrator' OR (SELECT rolsuper FROM pg_roles WHERE rolname=session_user)) AND pg_has_role(session_user,'billing_schema_owner','MEMBER') AND NOT pg_has_role('billing_service','billing_schema_owner','MEMBER') AND pg_has_role('billing_schema_owner','billing_service','USAGE') AND NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname IN ('billing_service','billing_schema_owner','billing_migrator') AND (rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls)) AND EXISTS(SELECT 1 FROM pg_roles WHERE rolname='billing_schema_owner' AND NOT rolcanlogin) AND NOT EXISTS(SELECT 1 FROM pg_roles WHERE (rolsuper OR rolcreaterole OR rolcreatedb OR rolreplication OR rolbypassrls) AND pg_has_role('billing_service',oid,'MEMBER'))`).Scan(&actor); err != nil {
		return err
	}
	if !actor {
		return errors.New("billing migration requires separate, provisioned deployment and owner identities")
	}
	var hardened bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('public.billing_runtime_hardening') IS NOT NULL`).Scan(&hardened); err != nil {
		return err
	}
	if migration.Version == 79 {
		if err := validateBonusDDL(migration); err != nil {
			return err
		}
		// Fresh079 precedes runtime hardening. The hardened upgrade path below
		// already borrows CREATE and object ownership for the same transaction.
		if !hardened {
			return protectBillingMigrationAndLendBonusDDL(ctx, tx, migration)
		}
	}
	if hardened {
		// This authority exists only in this uncommitted migration transaction.
		// The runtime is never granted membership in the independent owner role.
		if _, err := tx.Exec(ctx, `SET ROLE billing_schema_owner;
GRANT CREATE ON SCHEMA public TO billing_service;
DO $$ DECLARE object RECORD; BEGIN
 FOR object IN SELECT c.relname,c.relkind FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname='public' AND pg_get_userbyid(c.relowner)='billing_schema_owner' AND c.relkind IN ('r','p','S','v','m')
 AND c.relname NOT IN ('schema_migrations','billing_runtime_hardening','billing_approval_command_activations','billing_approval_command_activation_history','billing_approval_proof_claims') LOOP
 EXECUTE format('ALTER %s public.%I OWNER TO billing_service',CASE object.relkind WHEN 'S' THEN 'SEQUENCE' WHEN 'v' THEN 'VIEW' WHEN 'm' THEN 'MATERIALIZED VIEW' ELSE 'TABLE' END,object.relname);
 END LOOP;
 FOR object IN SELECT p.oid::regprocedure::text AS signature FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
 WHERE n.nspname='public' AND pg_get_userbyid(p.proowner)='billing_schema_owner'
 AND p.proname NOT IN ('harden_billing_runtime_objects','reject_billing_truncate') LOOP
 EXECUTE format('ALTER FUNCTION %s OWNER TO billing_service',object.signature);
 END LOOP;
END $$;
SET ROLE billing_service;`); err != nil {
			return err
		}
	}
	return protectLegacyRefundMigration(ctx, tx, migration)
}

func finishBillingMigration(ctx context.Context, tx pgx.Tx, migration migrations.Migration) error {
	if err := revokeBonusDDL(ctx, tx, migration); err != nil {
		return err
	}
	var hardened bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('public.billing_runtime_hardening') IS NOT NULL`).Scan(&hardened); err != nil {
		return err
	}
	if !hardened {
		return nil
	}
	// The frozen generic hardener predates these correction receipts. Preserve
	// their registered append-only INSERT/SELECT contract after every later migration,
	// rather than changing installed SQL or relying only on row triggers.
	_, err := tx.Exec(ctx, `SET ROLE billing_schema_owner;
SELECT public.harden_billing_runtime_objects();
DO $$ BEGIN
 IF to_regclass('public.entitlement_component_corrections') IS NOT NULL THEN
  REVOKE UPDATE,DELETE ON TABLE public.entitlement_component_corrections FROM billing_service;
 END IF;
 IF to_regclass('public.ledger_repair_cases') IS NOT NULL THEN
  REVOKE UPDATE,DELETE ON TABLE public.ledger_repair_cases FROM billing_service;
 END IF;
 IF to_regclass('public.receivable_write_off_cases') IS NOT NULL THEN
  REVOKE UPDATE,DELETE ON TABLE public.receivable_write_off_cases FROM billing_service;
 END IF;
 IF to_regclass('public.payment_order_repairs') IS NOT NULL THEN
  REVOKE UPDATE,DELETE ON TABLE public.payment_order_repairs FROM billing_service;
 END IF;
END $$;
SET ROLE billing_service;`)
	return err
}
