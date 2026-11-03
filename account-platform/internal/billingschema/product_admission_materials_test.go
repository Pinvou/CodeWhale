package billingschema

import (
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"testing"
)

func TestProductAdmissionMaterialMigrationIdentity(t *testing.T) {
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	var target migrations.Migration
	for _, m := range loaded {
		if m.Version == productAdmissionMaterialVersion {
			target = m
		}
	}
	if target.Version == 0 {
		t.Fatal("new material migration missing")
	}
	if err = validateProductAdmissionMaterials([]migrations.Migration{target}); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*migrations.Migration){func(m *migrations.Migration) { m.Version++ }, func(m *migrations.Migration) { m.Name = "other" }, func(m *migrations.Migration) { m.UpSQL += "-- change" }, func(m *migrations.Migration) { m.DownSQL += "-- change" }, func(m *migrations.Migration) { m.Checksum = "unknown" }} {
		changed := target
		mutate(&changed)
		if err = validateProductAdmissionMaterials([]migrations.Migration{changed}); err == nil {
			t.Fatal("changed identity accepted")
		}
	}
}
