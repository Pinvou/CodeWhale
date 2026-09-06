package token

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
)

// loadTokenIntegrationMigrations preserves the production migration's fixed
// public-schema security boundary while allowing the package's parallel
// integration tests to run in disposable schemas. Only the migration added by
// APT-123-R2 is schema-qualified; all older migrations already follow the
// connection search_path.
func loadTokenIntegrationMigrations(directory, schema string) ([]migrations.Migration, error) {
	loaded, err := migrations.Load(directory)
	if err != nil || schema == "" || schema == "public" {
		return loaded, err
	}
	qualified := pgx.Identifier{schema}.Sanitize()
	for index := range loaded {
		if loaded[index].Version != 29 {
			continue
		}
		loaded[index].UpSQL = "ALTER SCHEMA " + qualified + " OWNER TO token_schema_owner;\n" + rewritePublicSchema(loaded[index].UpSQL, qualified) + "\nRESET ROLE;"
		loaded[index].DownSQL = rewritePublicSchema(loaded[index].DownSQL, qualified)
		normalized := strings.ReplaceAll(strings.ReplaceAll(loaded[index].UpSQL, "\r\n", "\n"), "\r", "\n")
		digest := sha256.Sum256([]byte(normalized))
		loaded[index].Checksum = hex.EncodeToString(digest[:])
	}
	return loaded, nil
}

func rewritePublicSchema(sql, qualified string) string {
	sql = strings.ReplaceAll(sql, "to_regnamespace('public')", "to_regnamespace('"+strings.Trim(qualified, "\"")+"')")
	sql = strings.ReplaceAll(sql, "SCHEMA public", "SCHEMA "+qualified)
	return strings.ReplaceAll(sql, "public.", qualified+".")
}
