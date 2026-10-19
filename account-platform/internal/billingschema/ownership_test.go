package billingschema

import (
	"context"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"testing"
)

func TestLegacyChargebackCompatibilityRejectsUnfrozenSQL(t *testing.T) {
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range loaded {
		if m.Version != 29 {
			continue
		}
		m.UpSQL += "\nSELECT 1;\n"
		m.Checksum = sqlChecksum(m.UpSQL)
		if err = applyBillingMigration(context.Background(), nil, m); err == nil {
			t.Fatal("mutated SQL accepted despite forged metadata")
		}
		return
	}
	t.Fatal("missing original 029")
}

func TestPackageIndexDDLRejectsUnfrozenMigration(t *testing.T) {
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	for _, original := range loaded {
		if original.Version != 80 {
			continue
		}
		if err := validatePackageIndexDDL(original); err != nil {
			t.Fatal("original index pins", err)
		}
		for name, mutate := range map[string]func(*migrations.Migration){
			"version":         func(m *migrations.Migration) { m.Version++ },
			"name":            func(m *migrations.Migration) { m.Name = "foreign" },
			"checksum":        func(m *migrations.Migration) { m.Checksum = "foreign" },
			"up":              func(m *migrations.Migration) { m.UpSQL += " " },
			"up-and-checksum": func(m *migrations.Migration) { m.UpSQL += " "; m.Checksum = sqlChecksum(m.UpSQL) },
			"down":            func(m *migrations.Migration) { m.DownSQL += " " },
		} {
			t.Run(name, func(t *testing.T) {
				bad := original
				mutate(&bad)
				// Nil tx proves rejection occurs before any attempt to borrow authority.
				if err := lendPackageIndexDDL(context.Background(), nil, bad); err == nil {
					t.Fatal("unfrozen index migration accepted")
				}
			})
		}
		return
	}
	t.Fatal("missing original 080")
}
