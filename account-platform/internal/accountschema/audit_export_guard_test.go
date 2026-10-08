package accountschema

import (
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"path/filepath"
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
