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
		if loaded[index].Version != 43 && loaded[index].Version != 53 && loaded[index].Version != 54 && loaded[index].Version != 55 && loaded[index].Version != 56 && loaded[index].Version != 57 && loaded[index].Version != 58 && loaded[index].Version != 59 && loaded[index].Version != 60 && loaded[index].Version != 61 {
			continue
		}
		loaded[index].UpSQL = rewriteBillingPublicSchema(loaded[index].UpSQL, qualified)
		if loaded[index].Version == 53 || loaded[index].Version == 54 {
			loaded[index].UpSQL = "ALTER SCHEMA " + qualified + " OWNER TO billing_schema_owner;\n" + loaded[index].UpSQL + "\nRESET ROLE;"
		}
		if loaded[index].Version == 54 {
			loaded[index].UpSQL = "GRANT USAGE ON SCHEMA " + qualified + " TO billing_service;\n" + loaded[index].UpSQL
		}
		if loaded[index].Version == 55 || loaded[index].Version == 56 || loaded[index].Version == 57 || loaded[index].Version == 58 || loaded[index].Version == 59 || loaded[index].Version == 60 || loaded[index].Version == 61 {
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
