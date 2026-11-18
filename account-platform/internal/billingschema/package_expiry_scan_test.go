package billingschema

import (
	"context"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

func TestPackageExpiryScanMigrationIdentity(t *testing.T) {
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	var original migrations.Migration
	for _, item := range loaded {
		if item.Version == packageExpiryScanVersion {
			original = item
		}
	}
	if original.Version == 0 || validatePackageExpiryScan([]migrations.Migration{original}) != nil {
		t.Fatal("published index identity required")
	}
	for _, change := range []struct {
		name string
		edit func(*migrations.Migration)
	}{
		{"up", func(m *migrations.Migration) { m.UpSQL += "\nSELECT 1;" }},
		{"down", func(m *migrations.Migration) { m.DownSQL = "DROP TABLE public.entitlement_packages;" }},
		{"checksum", func(m *migrations.Migration) { m.Checksum = "changed" }},
		{"version", func(m *migrations.Migration) { m.Version++ }},
		{"name", func(m *migrations.Migration) { m.Name = "renamed" }},
		{"metadata-forged", func(m *migrations.Migration) { m.Version++; m.Name = "renamed"; m.Checksum = "changed" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			modified := original
			change.edit(&modified)
			if validatePackageExpiryScan([]migrations.Migration{modified}) == nil {
				t.Fatal("changed owner-role migration accepted")
			}
		})
	}
	if validatePackageExpiryScan([]migrations.Migration{original, original}) == nil {
		t.Fatal("duplicate owner-role index migration accepted")
	}
	crlf := original
	crlf.UpSQL = strings.ReplaceAll(crlf.UpSQL, "\n", "\r\n")
	crlf.DownSQL = strings.ReplaceAll(crlf.DownSQL, "\n", "\r\n")
	if err := validatePackageExpiryScan([]migrations.Migration{crlf}); err != nil {
		t.Fatal("line endings changed identity", err)
	}
	if err := validatePackageExpiryScan(nil); err != nil {
		t.Fatal("earlier artifact gained a new dependency", err)
	}
	for i := range loaded {
		if loaded[i].Version == packageExpiryScanVersion {
			loaded[i].DownSQL = "DROP TABLE public.entitlement_packages;"
		}
	}
	for name, run := range map[string]func() error{
		"up":     func() error { return Up(context.Background(), nil, loaded) },
		"down":   func() error { return DownOne(context.Background(), nil, loaded) },
		"verify": func() error { _, err := VerifyApplied(context.Background(), nil, loaded); return err },
	} {
		t.Run(name+"-before-db", func(t *testing.T) {
			if err := run(); err == nil || !strings.Contains(err.Error(), "package expiry scan") {
				t.Fatal("tampered artifact reached DB", err)
			}
		})
	}
}
