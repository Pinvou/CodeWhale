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
	if original.Version == 0 || validateLatePaymentQueryAdmission([]migrations.Migration{original}) != nil {
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
				expected = "migration 900046 rollback differs from published package"
			}
			if err := run(); err == nil || (name == "verify" && err.Error() != expected) || (name != "verify" && !strings.Contains(err.Error(), expected)) {
				t.Fatal("tampered artifact reached DB", err)
			}
		})
	}
}
