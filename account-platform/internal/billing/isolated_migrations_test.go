package billing

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
)

func loadBillingIntegrationMigrations(directory, schema string) ([]migrations.Migration, error) {
	loaded, err := migrations.Load(directory)
	if err != nil || schema == "" || schema == "public" {
		return loaded, err
	}
	qualified := pgx.Identifier{schema}.Sanitize()
	for index := range loaded {
		if loaded[index].Version != 43 && loaded[index].Version != 53 && loaded[index].Version != 54 && loaded[index].Version != 55 && loaded[index].Version != 56 && loaded[index].Version != 57 && loaded[index].Version != 58 && loaded[index].Version != 59 && loaded[index].Version != 60 && loaded[index].Version != 61 && loaded[index].Version != 62 && loaded[index].Version != 63 && loaded[index].Version != 64 && loaded[index].Version != 65 && loaded[index].Version != 66 && loaded[index].Version != 67 && loaded[index].Version != 68 && loaded[index].Version != 69 && loaded[index].Version != 70 && loaded[index].Version != 71 && loaded[index].Version != 72 && loaded[index].Version != 73 && loaded[index].Version != 74 && loaded[index].Version != 75 && loaded[index].Version != 76 && loaded[index].Version != 77 {
			continue
		}
		loaded[index].UpSQL = rewriteBillingPublicSchema(loaded[index].UpSQL, qualified)
		if loaded[index].Version == 53 || loaded[index].Version == 54 {
			loaded[index].UpSQL = "ALTER SCHEMA " + qualified + " OWNER TO billing_schema_owner;\n" + loaded[index].UpSQL + "\nRESET ROLE;"
		}
		if loaded[index].Version == 54 {
			loaded[index].UpSQL = "GRANT USAGE ON SCHEMA " + qualified + " TO billing_service;\n" + loaded[index].UpSQL
		}
		if loaded[index].Version == 55 || loaded[index].Version == 56 || loaded[index].Version == 57 || loaded[index].Version == 58 || loaded[index].Version == 59 || loaded[index].Version == 60 || loaded[index].Version == 61 || loaded[index].Version == 62 || loaded[index].Version == 63 || loaded[index].Version == 64 || loaded[index].Version == 65 || loaded[index].Version == 66 || loaded[index].Version == 67 || loaded[index].Version == 68 || loaded[index].Version == 69 || loaded[index].Version == 70 || loaded[index].Version == 71 || loaded[index].Version == 72 || loaded[index].Version == 73 || loaded[index].Version == 74 || loaded[index].Version == 75 || loaded[index].Version == 76 || loaded[index].Version == 77 {
			loaded[index].UpSQL += "\nRESET ROLE;"
		}
		if loaded[index].Version == 56 {
			for _, table := range []string{"entitlement_packages", "wallets", "payment_orders", "payment_transactions", "entitlement_components", "credit_lots", "journal_entries"} {
				loaded[index].UpSQL = "ALTER TABLE " + qualified + "." + table + " OWNER TO billing_service;\n" + loaded[index].UpSQL
			}
		}
		if loaded[index].Version == 60 {
			for _, table := range []string{"journal_entries", "receivables"} {
				loaded[index].UpSQL = "ALTER TABLE " + qualified + "." + table + " OWNER TO billing_service;\n" + loaded[index].UpSQL
			}
		}
		if loaded[index].Version == 61 {
			for _, table := range []string{"ledger_accounts", "ledger_postings", "journal_entries"} {
				loaded[index].UpSQL = "ALTER TABLE " + qualified + "." + table + " OWNER TO billing_service;\n" + loaded[index].UpSQL
			}
		}
		// 000063 grants REFERENCES on the receivable and ledger tables to the
		// schema owner, so the isolated runtime must own them first (same reason
		// as the 000060/000061 preludes above).
		if loaded[index].Version == 63 {
			for _, table := range []string{"receivables", "receivable_allocations", "journal_entries", "wallets"} {
				loaded[index].UpSQL = "ALTER TABLE " + qualified + "." + table + " OWNER TO billing_service;\n" + loaded[index].UpSQL
			}
		}
		// 000065 alters the audited repair table in place, so the isolated
		// runtime must own it first (same reason as the 000063 prelude above).
		if loaded[index].Version == 65 {
			loaded[index].UpSQL = "ALTER TABLE " + qualified + ".payment_order_repairs OWNER TO billing_service;\n" + loaded[index].UpSQL
		}
		// 000067 grants REFERENCES on the refund/anomaly/ledger tables to the schema
		// owner, so the isolated runtime must own them first.
		if loaded[index].Version == 67 {
			for _, table := range []string{"refunds", "payment_anomaly_cases", "wallets", "entitlement_packages", "payment_evidence", "journal_entries", "receivables"} {
				loaded[index].UpSQL = "ALTER TABLE " + qualified + "." + table + " OWNER TO billing_service;\n" + loaded[index].UpSQL
			}
		}
		// 000069 drops a constraint from the package table; the isolated runtime
		// owner must be able to see it as the table owner.
		if loaded[index].Version == 69 {
			loaded[index].UpSQL = "ALTER TABLE " + qualified + ".entitlement_packages OWNER TO billing_service;\n" + loaded[index].UpSQL
		}
		// 000070 grants REFERENCES on the chargeback/anomaly/ledger tables to the
		// schema owner, so the isolated runtime must own them first (same reason as
		// the 000067 prelude above).
		if loaded[index].Version == 70 {
			for _, table := range []string{"chargeback_cases", "payment_anomaly_cases", "wallets", "entitlement_packages", "payment_evidence", "journal_entries", "receivables"} {
				loaded[index].UpSQL = "ALTER TABLE " + qualified + "." + table + " OWNER TO billing_service;\n" + loaded[index].UpSQL
			}
		}
		// 000072 grants REFERENCES on the reconciliation/evidence tables to the
		// schema owner, so the isolated runtime must own them first (same reason as
		// the 000067/000070 preludes above).
		if loaded[index].Version == 72 {
			for _, table := range []string{"reconciliation_cases", "reconciliation_runs", "payment_evidence"} {
				loaded[index].UpSQL = "ALTER TABLE " + qualified + "." + table + " OWNER TO billing_service;\n" + loaded[index].UpSQL
			}
		}
		// 000074 grants REFERENCES on the usage/ledger tables to the schema owner,
		// so the isolated runtime must own them first (same reason as the
		// 000067/000070/000072 preludes above).
		if loaded[index].Version == 74 {
			for _, table := range []string{"usage_holds", "usage_loss_reviews", "wallets", "journal_entries", "receivables"} {
				loaded[index].UpSQL = "ALTER TABLE " + qualified + "." + table + " OWNER TO billing_service;\n" + loaded[index].UpSQL
			}
		}
		// 000077 alters the audited repair table in place (the requester column),
		// so the isolated runtime must own it first, exactly like the 000065
		// prelude above.
		if loaded[index].Version == 77 {
			loaded[index].UpSQL = "ALTER TABLE " + qualified + ".payment_order_repairs OWNER TO billing_service;\n" + loaded[index].UpSQL
		}
		loaded[index].DownSQL = rewriteBillingPublicSchema(loaded[index].DownSQL, qualified)
		normalized := strings.ReplaceAll(strings.ReplaceAll(loaded[index].UpSQL, "\r\n", "\n"), "\r", "\n")
		digest := sha256.Sum256([]byte(normalized))
		loaded[index].Checksum = hex.EncodeToString(digest[:])
	}
	return loaded, nil
}

func rewriteBillingPublicSchema(sql, qualified string) string {
	sql = strings.ReplaceAll(sql, "to_regnamespace('public')", "to_regnamespace('"+strings.Trim(qualified, "\"")+"')")
	sql = strings.ReplaceAll(sql, "SCHEMA public", "SCHEMA "+qualified)
	sql = strings.ReplaceAll(sql, "search_path=public,pg_temp", "search_path="+qualified+",pg_temp")
	return strings.ReplaceAll(sql, "public.", qualified+".")
}
