package billingschema

import (
	"context"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

func TestLatePaymentQueryGenerationsMigrationIdentity(t *testing.T) {
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	var original migrations.Migration
	for _, item := range loaded {
		if item.Version == latePaymentQueryGenerationsVersion {
			original = item
		}
	}
	if original.Version != 900048 || validateLatePaymentQueryGenerations([]migrations.Migration{original}) != nil {
		t.Fatal("registered immutable query-generation binding required")
	}
	for _, change := range []struct {
		name string
		edit func(*migrations.Migration)
	}{
		{"up", func(m *migrations.Migration) { m.UpSQL += "\nSELECT 1;" }},
		{"down", func(m *migrations.Migration) { m.DownSQL = "DROP TABLE public.late_payment_query_generations;" }},
		{"checksum", func(m *migrations.Migration) { m.Checksum = "changed" }},
		{"version", func(m *migrations.Migration) { m.Version++ }},
		{"reserved-admission-version", func(m *migrations.Migration) { m.Version = 900047 }},
		{"name", func(m *migrations.Migration) { m.Name = "renamed" }},
		{"metadata-forged", func(m *migrations.Migration) { m.Version++; m.Name = "renamed"; m.Checksum = "changed" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			modified := original
			change.edit(&modified)
			if validateLatePaymentQueryGenerations([]migrations.Migration{modified}) == nil {
				t.Fatal("changed generation binding identity accepted")
			}
		})
	}
	if validateLatePaymentQueryGenerations([]migrations.Migration{original, original}) == nil {
		t.Fatal("duplicate generation binding identity accepted")
	}
	crlf := original
	crlf.UpSQL = strings.ReplaceAll(crlf.UpSQL, "\n", "\r\n")
	crlf.DownSQL = strings.ReplaceAll(crlf.DownSQL, "\n", "\r\n")
	if err := validateLatePaymentQueryGenerations([]migrations.Migration{crlf}); err != nil {
		t.Fatal("line endings changed the frozen identity", err)
	}
	if err := validateLatePaymentQueryGenerations(nil); err != nil {
		t.Fatal("unrelated older carrier gained a mandatory migration", err)
	}
	for _, unrelated := range []migrations.Migration{
		{Version: 900046, Name: "outbox_dead_statistics_index", Checksum: "unrelated-statistics"},
		{Version: 900047, Name: "late_payment_query_admission", Checksum: latePaymentQueryAdmissionUp},
	} {
		if err := validateLatePaymentQueryGenerations([]migrations.Migration{unrelated, original}); err != nil {
			t.Fatal("binding identity captured an unrelated migration", err)
		}
	}
	wrong := original
	wrong.Version = 900049
	for name, run := range map[string]func() error{
		"up":   func() error { return Up(context.Background(), nil, []migrations.Migration{wrong}) },
		"down": func() error { return DownOne(context.Background(), nil, []migrations.Migration{wrong}) },
		"verify": func() error {
			_, err := VerifyApplied(context.Background(), nil, []migrations.Migration{wrong})
			return err
		},
	} {
		t.Run(name+"-reserved-version-before-db", func(t *testing.T) {
			if err := run(); err == nil || !strings.Contains(err.Error(), "late payment query generations") {
				t.Fatal("misnumbered generation binding reached DB")
			}
		})
	}
	for i := range loaded {
		if loaded[i].Version == latePaymentQueryGenerationsVersion {
			loaded[i].DownSQL = "DROP TABLE public.late_payment_query_generations;"
		}
	}
	for name, run := range map[string]func() error{
		"up":     func() error { return Up(context.Background(), nil, loaded) },
		"down":   func() error { return DownOne(context.Background(), nil, loaded) },
		"verify": func() error { _, err := VerifyApplied(context.Background(), nil, loaded); return err },
	} {
		t.Run(name+"-before-db", func(t *testing.T) {
			err := run()
			if name == "verify" {
				if err == nil || err.Error() != "migration 900048 rollback differs from published package" {
					t.Fatal("published rollback error priority changed", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "late payment query generations") {
				t.Fatal("tampered binding artifact reached DB", err)
			}
		})
	}
}
