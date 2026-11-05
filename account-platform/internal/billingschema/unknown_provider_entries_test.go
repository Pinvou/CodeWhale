package billingschema

import (
	"context"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"strings"
	"testing"
)

func TestUnknownProviderEntryMigrationPinsBeforeDatabase(t *testing.T) {
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	var original migrations.Migration
	for _, m := range loaded {
		if m.Version == unknownProviderEntryVersion {
			original = m
		}
	}
	if original.Version == 0 {
		t.Fatal("new forward migration missing")
	}
	for name, mutate := range map[string]func(*migrations.Migration){
		"up-body":   func(m *migrations.Migration) { m.UpSQL += "\n-- changed" },
		"down-body": func(m *migrations.Migration) { m.DownSQL = "SELECT 1;" },
		"identity":  func(m *migrations.Migration) { m.Version = 900026 },
		"name":      func(m *migrations.Migration) { m.Name += "_changed" },
	} {
		t.Run(name, func(t *testing.T) {
			copy := append([]migrations.Migration(nil), loaded...)
			for i := range copy {
				if copy[i].Version == original.Version {
					mutate(&copy[i])
				}
			}
			if _, err := Ordered(copy); err == nil {
				t.Fatal("changed append-only input accepted")
			}
			if err := Up(context.Background(), nil, copy); err == nil {
				t.Fatal("invalid migration reached DB")
			}
		})
	}
	if !strings.Contains(original.DownSQL, "RAISE EXCEPTION") {
		t.Fatal("rollback discards source evidence")
	}
}
