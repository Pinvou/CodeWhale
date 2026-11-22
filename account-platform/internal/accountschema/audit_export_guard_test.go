package accountschema

import (
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuditExportMigrationPinsBothOriginalDirections(t *testing.T) {
	loaded, err := migrations.Load(filepath.Join("..", "..", "migrations", "account"))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateAuditExportMigration(loaded); err != nil {
		t.Fatal(err)
	}
	for i, m := range loaded {
		if m.Version != 32 {
			continue
		}
		changed := append([]migrations.Migration(nil), loaded...)
		changed[i].DownSQL += "\nSELECT 1;"
		if validateAuditExportMigration(changed) == nil {
			t.Fatal("edited Down accepted")
		}
		changed = append([]migrations.Migration(nil), loaded...)
		changed[i].UpSQL += "\nSELECT 1;"
		if validateAuditExportMigration(changed) == nil {
			t.Fatal("edited Up accepted")
		}
		return
	}
	t.Fatal("audit migration absent")
}

// Both public directions pass through Ordered before opening a transaction.
// Directly testing the private digest helper cannot prove that admission.
func TestAuditExportPublicOrderingPinsOriginalSQL(t *testing.T) {
	loaded, err := migrations.Load(filepath.Join("..", "..", "migrations", "account"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Ordered(loaded); err != nil {
		t.Fatal(err)
	}
	for i, m := range loaded {
		if m.Version != 32 {
			continue
		}
		for _, direction := range []string{"up", "down", "forged_up_checksum"} {
			t.Run(direction, func(t *testing.T) {
				changed := append([]migrations.Migration(nil), loaded...)
				if direction == "down" {
					changed[i].DownSQL += "\nSELECT 1;"
				} else {
					changed[i].UpSQL += "\nSELECT 1;"
				}
				if direction == "forged_up_checksum" {
					changed[i].Checksum = "forged"
				}
				if _, err := Ordered(changed); err == nil {
					t.Fatal("public ordering accepted edited audit export SQL")
				}
			})
		}
		t.Run("canonical_line_endings", func(t *testing.T) {
			changed := append([]migrations.Migration(nil), loaded...)
			changed[i].UpSQL = strings.ReplaceAll(strings.ReplaceAll(m.UpSQL, "\r\n", "\n"), "\n", "\r\n")
			changed[i].DownSQL = strings.ReplaceAll(strings.ReplaceAll(m.DownSQL, "\r\n", "\n"), "\n", "\r\n")
			if _, err := Ordered(changed); err != nil {
				t.Fatal("canonical line endings rejected", err)
			}
		})
		return
	}
	t.Fatal("audit migration absent")
}
