package billingschema

import (
	"context"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

func TestLatePaymentQueryAdmissionMigrationIdentity(t *testing.T) {
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	var original migrations.Migration
	for _, item := range loaded {
		if item.Version == latePaymentQueryAdmissionVersion {
			original = item
		}
	}
	if original.Version != 900047 || validateLatePaymentQueryAdmission([]migrations.Migration{original}) != nil {
		t.Fatal("published current-query identity required")
	}
	for _, change := range []struct {
		name string
		edit func(*migrations.Migration)
	}{
		{"up", func(m *migrations.Migration) { m.UpSQL += "\nSELECT 1;" }},
		{"down", func(m *migrations.Migration) { m.DownSQL = "DROP TABLE public.payment_orders;" }},
		{"checksum", func(m *migrations.Migration) { m.Checksum = "changed" }},
		{"version", func(m *migrations.Migration) { m.Version++ }},
		{"reserved-outbox-version", func(m *migrations.Migration) { m.Version = 900046 }},
		{"name", func(m *migrations.Migration) { m.Name = "renamed" }},
		{"metadata-forged", func(m *migrations.Migration) { m.Version++; m.Name = "renamed"; m.Checksum = "changed" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			modified := original
			change.edit(&modified)
			if validateLatePaymentQueryAdmission([]migrations.Migration{modified}) == nil {
				t.Fatal("changed owner-role migration accepted")
			}
		})
	}
	if validateLatePaymentQueryAdmission([]migrations.Migration{original, original}) == nil {
		t.Fatal("duplicate current-query migration accepted")
	}
	crlf := original
	crlf.UpSQL = strings.ReplaceAll(crlf.UpSQL, "\n", "\r\n")
	crlf.DownSQL = strings.ReplaceAll(crlf.DownSQL, "\n", "\r\n")
	if err := validateLatePaymentQueryAdmission([]migrations.Migration{crlf}); err != nil {
		t.Fatal("line endings changed identity", err)
	}
	if err := validateLatePaymentQueryAdmission(nil); err != nil {
		t.Fatal("earlier artifact gained a new dependency", err)
	}
	// This is an unrelated identity sentinel, not executable outbox SQL.
	// The independent statistics migration keeps 900046; its own guard
	// remains responsible for its SQL and permissions.
	other := migrations.Migration{Version: 900046, Name: "outbox_dead_statistics_index", Checksum: "fdb45f949ce1fe97935022f6ffe057003243ba5b5cd1c6e863ffcb32cf1fed40"}
	if err := validateLatePaymentQueryAdmission([]migrations.Migration{other, original}); err != nil {
		t.Fatal("unrelated reserved version captured by query identity", err)
	}
	wrong := original
	wrong.Version = 900046
	for name, run := range map[string]func() error{
		"up":   func() error { return Up(context.Background(), nil, []migrations.Migration{wrong}) },
		"down": func() error { return DownOne(context.Background(), nil, []migrations.Migration{wrong}) },
		"verify": func() error {
			_, err := VerifyApplied(context.Background(), nil, []migrations.Migration{wrong})
			return err
		},
	} {
		t.Run(name+"-reserved-version-before-db", func(t *testing.T) {
			if err := run(); err == nil || !strings.Contains(err.Error(), "late payment query admission") {
				t.Fatal("misnumbered query identity reached DB", err)
			}
		})
	}
	for i := range loaded {
		if loaded[i].Version == latePaymentQueryAdmissionVersion {
			loaded[i].DownSQL = "DROP TABLE public.payment_orders;"
		}
	}
	for name, run := range map[string]func() error{
		"up":     func() error { return Up(context.Background(), nil, loaded) },
		"down":   func() error { return DownOne(context.Background(), nil, loaded) },
		"verify": func() error { _, err := VerifyApplied(context.Background(), nil, loaded); return err },
	} {
		t.Run(name+"-before-db", func(t *testing.T) {
			expected := "late payment query admission"
			if name == "verify" {
				expected = "migration 900047 rollback differs from published package"
			}
			if err := run(); err == nil || (name == "verify" && err.Error() != expected) || (name != "verify" && !strings.Contains(err.Error(), expected)) {
				t.Fatal("tampered artifact reached DB", err)
			}
		})
	}
}
