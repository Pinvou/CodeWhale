package tokenschema

import (
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"testing"
)

func TestInstalledManagementMigrationPackage(t *testing.T) {
	loaded, err := migrations.Load("../../migrations/token")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 36 {
		t.Fatalf("complete installed Token migration package has %d identities, want 36", len(loaded))
	}
	if _, err = Ordered(loaded); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func([]migrations.Migration) []migrations.Migration
	}{
		{"missing", func(ms []migrations.Migration) []migrations.Migration { return ms[1:] }},
		{"future-version", func(ms []migrations.Migration) []migrations.Migration { ms[0].Version = 35; return ms }},
		{"duplicate", func(ms []migrations.Migration) []migrations.Migration { ms[1] = ms[0]; return ms }},
		{"modified-up", func(ms []migrations.Migration) []migrations.Migration { ms[0].UpSQL += "\nSELECT 1;"; return ms }},
		{"modified-down", func(ms []migrations.Migration) []migrations.Migration { ms[0].DownSQL += "\nSELECT 1;"; return ms }},
		{"wrong-name", func(ms []migrations.Migration) []migrations.Migration { ms[0].Name = "other"; return ms }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := append([]migrations.Migration(nil), loaded...)
			if _, err := Ordered(tc.change(copy)); err == nil {
				t.Fatal("uninstalled or modified migration accepted")
			}
		})
	}
}
