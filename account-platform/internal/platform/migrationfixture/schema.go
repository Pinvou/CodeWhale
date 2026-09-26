// Package migrationfixture contains schema-isolation transforms used only by
// PostgreSQL integration fixtures. Production migrations must always execute
// with their original SQL and checksum.
package migrationfixture

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
)

// RewritePublicSchema moves a role-aware migration from public into a
// disposable integration-test schema and recalculates only the fixture copy's
// checksum. The caller remains responsible for selecting the migration
// versions whose SQL has a fixed public-schema boundary.
func RewritePublicSchema(migration migrations.Migration, schema string) migrations.Migration {
	qualified := pgx.Identifier{schema}.Sanitize()
	prefix := "ALTER SCHEMA " + qualified + " OWNER TO token_schema_owner;\n"
	if migration.Version == 34 {
		// Production usage_requests is owned by token_gateway and public grants
		// runtime USAGE. Disposable schemas start with neither property, so mirror
		// both inside the private schema before exercising the same trigger/FK
		// owner chain. No public object or ACL is touched.
		prefix += "GRANT USAGE ON SCHEMA " + qualified + " TO token_gateway;\n" +
			"ALTER TABLE " + qualified + ".usage_requests OWNER TO token_gateway;\n"
	}
	migration.UpSQL = prefix + rewritePublicReferences(migration.UpSQL, schema, qualified) + "\nRESET ROLE;"
	migration.DownSQL = rewritePublicReferences(migration.DownSQL, schema, qualified)
	normalized := strings.ReplaceAll(strings.ReplaceAll(migration.UpSQL, "\r\n", "\n"), "\r", "\n")
	digest := sha256.Sum256([]byte(normalized))
	migration.Checksum = hex.EncodeToString(digest[:])
	return migration
}

func rewritePublicReferences(sql, schema, qualified string) string {
	schemaLiteral := strings.ReplaceAll(schema, "'", "''")
	sql = strings.ReplaceAll(sql, "to_regnamespace('public')", "to_regnamespace('"+schemaLiteral+"')")
	sql = strings.ReplaceAll(sql, "SCHEMA public", "SCHEMA "+qualified)
	return strings.ReplaceAll(sql, "public.", qualified+".")
}
