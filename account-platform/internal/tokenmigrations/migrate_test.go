package tokenmigrations

import (
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

func TestPublishedMigration33Identity(t *testing.T) {
	loaded, err := migrations.Load("../../migrations/token")
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range loaded {
		if migration.Version != legacyOverviewIndexVersion {
			continue
		}
		if migration.Name != legacyOverviewIndexName || migrationDigest(migration.UpSQL) != legacyOverviewIndexSHA256 {
			t.Fatalf("published migration 33 identity drifted: name=%q digest=%q", migration.Name, migrationDigest(migration.UpSQL))
		}
		if migrationDigest(migration.UpSQL+"\nSELECT 1;") == legacyOverviewIndexSHA256 {
			t.Fatal("migration 33 body mutation retained the privileged-hook identity")
		}
		return
	}
	t.Fatal("published migration 33 is missing")
}
