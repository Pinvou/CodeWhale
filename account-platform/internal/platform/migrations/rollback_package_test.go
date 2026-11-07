package migrations

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishedRollbackPackagePins(t *testing.T) {
	for _, domain := range []string{"account", "billing", "payment-simulator", "token"} {
		t.Run(domain, func(t *testing.T) {
			loaded, err := Load(filepath.Join("..", "..", "..", "migrations", domain))
			if err != nil || len(loaded) == 0 {
				t.Fatal("load actual published package", err)
			}
			for _, migration := range loaded {
				identity := rollbackIdentity{version: migration.Version, up: normalizedChecksum(migration.UpSQL)}
				if _, known := publishedRollbackChecksums[identity]; !known {
					t.Fatalf("published migration absent from embedded inventory: %s/%d", domain, migration.Version)
				}
				changed := migration
				changed.DownSQL += "\n-- changed after publication\n"
				if err := verifyPublishedRollbacks([]Migration{changed}); err == nil {
					t.Fatalf("changed published rollback accepted: %s/%d", domain, migration.Version)
				}
				crlf := migration
				crlf.UpSQL = strings.ReplaceAll(normalizeLineEndings(crlf.UpSQL), "\n", "\r\n")
				crlf.DownSQL = strings.ReplaceAll(normalizeLineEndings(crlf.DownSQL), "\n", "\r\n")
				if err := verifyPublishedRollbacks([]Migration{crlf}); err != nil {
					t.Fatal("equivalent checkout line endings rejected", err)
				}
			}
			// A copied pair has the same published identity even when mounted under
			// an unrelated directory. Path names must not bypass rollback integrity.
			first := loaded[0]
			copyDir := t.TempDir()
			writeMigration(t, copyDir, "000001_copied.up.sql", first.UpSQL)
			writeMigration(t, copyDir, "000001_copied.down.sql", first.DownSQL)
			if _, err := Load(copyDir); err != nil {
				t.Fatal("unchanged copied package rejected", err)
			}
			if err := os.WriteFile(filepath.Join(copyDir, "000001_copied.down.sql"), []byte("SELECT 'changed rollback';"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(copyDir); err == nil {
				t.Fatal("changed copied rollback accepted")
			}
		})
	}
}

func TestUnrelatedCustomRollbackPackageRemainsGeneric(t *testing.T) {
	custom := Migration{Version: 1, Name: "custom_history", UpSQL: "SELECT 'custom forward';", DownSQL: "SELECT 'custom reverse';"}
	if err := verifyPublishedRollbacks([]Migration{custom}); err != nil {
		t.Fatal("unrelated custom history acquired a public-package dependency", err)
	}
	custom.DownSQL = "SELECT 'changed custom reverse';"
	if err := verifyPublishedRollbacks([]Migration{custom}); err != nil {
		t.Fatal("inventory falsely attested custom historical rollback bytes", err)
	}
}
