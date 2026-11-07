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
				changed.DownSQL = ""
				if !changed.HasDownScript || verifyPublishedRollbacks([]Migration{changed}) == nil {
					t.Fatalf("supplied empty published rollback accepted: %s/%d", domain, migration.Version)
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
			if err := os.WriteFile(filepath.Join(copyDir, "000001_copied.down.sql"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(copyDir); err == nil {
				t.Fatal("present empty copied rollback accepted")
			}
		})
	}
}

func TestLoadPublishedForwardOnlyHistory(t *testing.T) {
	loaded, err := Load(filepath.Join("..", "..", "..", "migrations", "billing"))
	if err != nil || len(loaded) == 0 {
		t.Fatal("actual published source required", err)
	}
	directory := t.TempDir()
	writeMigration(t, directory, "000001_foundation.up.sql", loaded[0].UpSQL)
	forwardOnly, err := Load(directory)
	if err != nil {
		t.Fatal("supported Up-only published history rejected", err)
	}
	if len(forwardOnly) != 1 || forwardOnly[0].HasDownScript || forwardOnly[0].DownSQL != "" || forwardOnly[0].Checksum != loaded[0].Checksum {
		t.Fatal("forward-only descriptor acquired rollback content or changed identity")
	}
	if err := verifyPublishedRollbacks(forwardOnly); err != nil {
		t.Fatal("forward-only descriptor rejected at shared entry", err)
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
