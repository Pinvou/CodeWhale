package token

import (
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrationfixture"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

// loadTokenIntegrationMigrations preserves the production migration's fixed
// public-schema security boundary while allowing the package's parallel
// integration tests to run in disposable schemas. Only the migration added by
// APT-123-R2 is schema-qualified; all older migrations already follow the
// connection search_path. The usage-history projection migration uses the same
// production owner/runtime split and therefore needs the same schema rewrite.
// Callers of this helper intentionally use migrations.Up directly: rewriting
// public to a disposable schema changes migration 33's immutable digest, so it
// is not a production role-chain path and must never receive the public-schema
// tokenmigrations compatibility grant.
func loadTokenIntegrationMigrations(directory, schema string) ([]migrations.Migration, error) {
	loaded, err := migrations.Load(directory)
	if err != nil || schema == "" || schema == "public" {
		return loaded, err
	}
	for index := range loaded {
		if loaded[index].Version != 29 && loaded[index].Version != 31 && loaded[index].Version != 32 && loaded[index].Version != 34 {
			continue
		}
		loaded[index] = migrationfixture.RewritePublicSchema(loaded[index], schema)
	}
	return loaded, nil
}
