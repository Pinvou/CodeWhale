package billingschema

import (
	"context"
	"errors"
	"fmt"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
	"strings"
)

// Original immutable 029 DDL excludes a former terminal state. Preserve old
// values byte-for-byte while applying its new-row check as NOT VALID.
const legacyAnomalyChecksum = "849e4ae4ff57132486992216005fc56a3f9a06c31a3cc84bc14c60c5fbe9bedb"
const chargebackStatusCheck = "ADD CONSTRAINT chargeback_cases_status_check CHECK (status IN ('REVIEW_REQUIRED','CONFIRMED','DISMISSED'))"

// Original 025 changed the expiry spelling and 060 rebuilds that CHECK.
// Preserve posted EXPIRE facts across both. Original 013 also allowed
// REPAIR_CREDIT, which 025 temporarily excludes until 060 restores that type.
// Apply only the exact retained checks to new rows; never rewrite posted facts.
const legacyExpiryChecksum = "a5015c7fdaa8948820967a887c556cf55ca70be5078d4378fb5457dca9dec23d"
const journalExpiryCheck = "ADD CONSTRAINT journal_entries_entry_type_check\n    CHECK (entry_type IN ('RECHARGE','USAGE','REFUND','REVERSAL','ADJUSTMENT','EXPIRY','RECEIVABLE','CHARGEBACK'))"
const repairedExpiryChecksum = "159061bfd00cd057249247e292a5aaa96242becd506f845731b866c0c10c8bfb"
const repairedJournalExpiryCheck = "ADD CONSTRAINT journal_entries_entry_type_check\n    CHECK (entry_type IN ('RECHARGE','USAGE','REFUND','REVERSAL','ADJUSTMENT','EXPIRY','RECEIVABLE','CHARGEBACK','REPAIR_CREDIT','FORCED_REFUND'))"

func validateLegacyExpiryMigration(input []migrations.Migration) error {
	for _, migration := range input {
		for _, identity := range []struct {
			version               int64
			name, checksum, check string
		}{
			{25, "receivables_and_context_freeze", legacyExpiryChecksum, journalExpiryCheck},
			{60, "repair_journal_and_receivable_enums", repairedExpiryChecksum, repairedJournalExpiryCheck},
		} {
			if migration.Version != identity.version && migration.Name != identity.name && migration.Checksum != identity.checksum {
				continue
			}
			sql := strings.ReplaceAll(strings.ReplaceAll(migration.UpSQL, "\r\n", "\n"), "\r", "\n")
			if migration.Version != identity.version || migration.Name != identity.name || migration.Checksum != identity.checksum || sqlChecksum(sql) != identity.checksum || strings.Count(sql, identity.check) != 1 {
				return fmt.Errorf("billing %03d expiry compatibility requires the exact retained migration", identity.version)
			}
		}
	}
	return nil
}

// Match the original effect migration, including the retained runner's legacy
// CRLF ledger representation. An unrelated version 58 cannot open the phase.
const entitlementEffectChecksum = "a6ff23901e5c01f83dfcb09ad6e0a4f4ec47b93658d094c4c3767ce92b2a45a7"
const entitlementEffectLegacyChecksum = "31d2dabe3d1f0c865c7db413279eaee60343c8e71ac1f17a6640cebb7afdf4df"

func applyBillingMigration(ctx context.Context, tx pgx.Tx, migration migrations.Migration) error {
	sql := migration.UpSQL
	if migration.Version == 25 || migration.Version == 60 {
		sql = strings.ReplaceAll(strings.ReplaceAll(sql, "\r\n", "\n"), "\r", "\n")
		if err := validateLegacyExpiryMigration([]migrations.Migration{migration}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `LOCK TABLE journal_entries IN ACCESS EXCLUSIVE MODE`); err != nil {
			return err
		}
		var legacy bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM journal_entries WHERE entry_type='EXPIRE' OR ($1::bigint=25 AND entry_type='REPAIR_CREDIT'))`, migration.Version).Scan(&legacy); err != nil {
			return err
		}
		if legacy {
			check := journalExpiryCheck
			if migration.Version == 60 {
				check = repairedJournalExpiryCheck
			}
			sql = strings.Replace(sql, check, check+" NOT VALID", 1)
		}
	}
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
	if _, err := knownLegacyWaiverHistory(ctx, tx); err != nil {
		return err
	}
	if _, err := knownOriginalWalletReceiptHistory(ctx, tx); err != nil {
		return err
	}
	var actor bool
	if err := tx.QueryRow(ctx, `SELECT (session_user='billing_migrator' OR (SELECT rolsuper FROM pg_roles WHERE rolname=session_user)) AND pg_has_role(session_user,'billing_schema_owner','MEMBER') AND NOT pg_has_role('billing_service','billing_schema_owner','MEMBER') AND pg_has_role('billing_schema_owner','billing_service','USAGE') AND NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname IN ('billing_service','billing_schema_owner','billing_migrator') AND (rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls)) AND EXISTS(SELECT 1 FROM pg_roles WHERE rolname='billing_schema_owner' AND NOT rolcanlogin) AND NOT EXISTS(SELECT 1 FROM pg_roles WHERE (rolsuper OR rolcreaterole OR rolcreatedb OR rolreplication OR rolbypassrls) AND pg_has_role('billing_service',oid,'MEMBER'))`).Scan(&actor); err != nil {
		return err
	}
	if !actor {
		return errors.New("billing migration requires separate, provisioned deployment and owner identities")
	}
	if err := protectExistingWalletReceiptHardening(ctx, tx); err != nil {
		return err
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
	if migration.Version == 80 {
		if err := validatePackageIndexDDL(migration); err != nil {
			return err
		}
		if !hardened {
			return lendPackageIndexDDL(ctx, tx, migration)
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
 AND c.relname NOT IN ('schema_migrations','billing_runtime_hardening','billing_approval_command_activations','billing_approval_command_activation_history','billing_approval_proof_claims','entitlement_component_corrections','token_account_wallet_provisioning_receipts','usage_hold_admin_commands','usage_waiver_commands','usage_waiver_results','usage_waiver_rejections','product_admission_materials','product_admission_material_ciphertexts','product_admission_material_commands','product_admission_material_audits','product_manual_admission_submissions','product_manual_admission_decisions') LOOP
 EXECUTE format('ALTER %s public.%I OWNER TO billing_service',CASE object.relkind WHEN 'S' THEN 'SEQUENCE' WHEN 'v' THEN 'VIEW' WHEN 'm' THEN 'MATERIALIZED VIEW' ELSE 'TABLE' END,object.relname);
 END LOOP;
 FOR object IN SELECT p.oid::regprocedure::text AS signature FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
 WHERE n.nspname='public' AND pg_get_userbyid(p.proowner)='billing_schema_owner'
 AND p.proname NOT IN ('harden_billing_runtime_objects','reject_billing_truncate','reject_token_account_wallet_receipt_mutation','guard_product_admission_material','guard_product_admission_ciphertext','purge_product_manual_admission_facts') LOOP
 EXECUTE format('ALTER FUNCTION %s OWNER TO billing_service',object.signature);
 END LOOP;
END $$;
SET ROLE billing_service;`); err != nil {
			return err
		}
	}
	if err := protectLegacyRefundMigration(ctx, tx, migration); err != nil {
		return err
	}
	if err := prepareManualProductAdmissionMigration(ctx, tx, migration); err != nil {
		return err
	}
	return prepareWalletReceiptValidation(ctx, tx, migration)
}

func finishBillingMigration(ctx context.Context, tx pgx.Tx, migration migrations.Migration) error {
	if err := finishManualProductAdmissionMigration(ctx, tx, migration); err != nil {
		return err
	}
	if err := revokePackageIndexDDL(ctx, tx, migration); err != nil {
		return err
	}
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
	// The original prepare phase is read-only; only installed 058 enables
	// append-only receipts. AfterApply runs before this migration's ledger write,
	// so the current 058 counts explicitly. Never infer a phase from MAX(version).
	effectInstalled := `EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=58 AND checksum IN ('` + entitlementEffectChecksum + `','` + entitlementEffectLegacyChecksum + `'))`
	if migration.Version == 58 && sqlChecksum(migration.UpSQL) == entitlementEffectChecksum {
		effectInstalled = `TRUE`
	}
	_, err := tx.Exec(ctx, `SET ROLE billing_schema_owner;
SELECT public.harden_billing_runtime_objects();
DO $$ BEGIN
 IF to_regclass('public.entitlement_component_corrections') IS NOT NULL THEN
  REVOKE UPDATE,DELETE ON TABLE public.entitlement_component_corrections FROM billing_service;
  IF `+effectInstalled+` THEN
   GRANT INSERT ON TABLE public.entitlement_component_corrections TO billing_service;
  ELSE
   REVOKE INSERT ON TABLE public.entitlement_component_corrections FROM billing_service;
  END IF;
 END IF;
 IF to_regclass('public.token_account_wallet_provisioning_receipts') IS NOT NULL AND (SELECT pg_get_userbyid(relowner)='billing_schema_owner' FROM pg_class WHERE oid=to_regclass('public.token_account_wallet_provisioning_receipts')) THEN
  REVOKE UPDATE,DELETE ON TABLE public.token_account_wallet_provisioning_receipts FROM billing_service;
 END IF;
 IF to_regclass('public.product_admission_materials') IS NOT NULL THEN
  REVOKE DELETE,TRUNCATE ON public.product_admission_materials FROM billing_service;
  REVOKE UPDATE,TRUNCATE ON public.product_admission_material_ciphertexts FROM billing_service;
  REVOKE UPDATE,DELETE,TRUNCATE ON public.product_admission_material_commands,public.product_admission_material_audits FROM billing_service;
 END IF;
 IF to_regclass('public.product_manual_admission_submissions') IS NOT NULL THEN
  IF to_regprocedure('public.purge_product_manual_admission_facts()') IS NOT NULL THEN
   ALTER FUNCTION public.purge_product_manual_admission_facts() OWNER TO billing_schema_owner;
   ALTER FUNCTION public.purge_product_manual_admission_facts() SECURITY DEFINER;
   ALTER FUNCTION public.purge_product_manual_admission_facts() SET search_path TO pg_catalog,public;
  END IF;
  REVOKE UPDATE,DELETE,TRUNCATE ON public.product_manual_admission_submissions,public.product_manual_admission_decisions FROM billing_service;
  REVOKE UPDATE(snapshot) ON public.product_manual_admission_submissions FROM billing_service;
  REVOKE UPDATE(reason) ON public.product_manual_admission_decisions FROM billing_service;
 END IF;
 IF to_regclass('public.usage_waiver_commands') IS NOT NULL THEN
  REVOKE UPDATE,DELETE,TRUNCATE ON public.usage_waiver_commands,public.usage_waiver_results,public.usage_waiver_rejections FROM billing_service;
 END IF;
 IF to_regclass('public.usage_hold_admin_commands') IS NOT NULL THEN
  REVOKE UPDATE,DELETE,TRUNCATE ON public.usage_hold_admin_commands FROM billing_service;
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
 IF to_regclass('public.refund_exception_repair_cases') IS NOT NULL THEN
  REVOKE UPDATE,DELETE ON TABLE public.refund_exception_repair_cases FROM billing_service;
 END IF;
 IF to_regclass('public.chargeback_financial_repair_cases') IS NOT NULL THEN
  REVOKE UPDATE,DELETE ON TABLE public.chargeback_financial_repair_cases FROM billing_service;
 END IF;
 IF to_regclass('public.usage_settlement_repair_cases') IS NOT NULL THEN
  REVOKE UPDATE,DELETE ON TABLE public.usage_settlement_repair_cases FROM billing_service;
 END IF;
 IF to_regclass('public.reconciliation_repair_cases') IS NOT NULL THEN
  REVOKE UPDATE,DELETE ON TABLE public.reconciliation_repair_cases FROM billing_service;
 END IF;
END $$;
SET ROLE billing_service;`)
	if err != nil {
		return err
	}
	return verifyWalletReceiptValidationRestored(ctx, tx, migration)
}
