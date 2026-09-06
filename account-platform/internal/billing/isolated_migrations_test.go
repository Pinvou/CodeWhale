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
		if loaded[index].Version != 53 {
			continue
		}
		loaded[index].UpSQL = "ALTER SCHEMA " + qualified + " OWNER TO billing_schema_owner;\n" + rewriteBillingPublicSchema(loaded[index].UpSQL, qualified) + "\nRESET ROLE;"
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
	return strings.ReplaceAll(sql, "public.", qualified+".")
}
