package accountschema

import (
	"context"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

func TestOrganizationEvidenceGuardPinsBothDirections(t *testing.T) {
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	var original migrations.Migration
	for _, m := range loaded {
		if m.Version == organizationEvidenceGuardVersion {
			original = m
		}
	}
	if original.Version == 0 || validateOrganizationEvidenceGuard([]migrations.Migration{original}) != nil {
		t.Fatal("actual published protection migration required")
	}
	for _, change := range []struct {
		name string
		edit func(*migrations.Migration)
	}{
		{"up", func(m *migrations.Migration) { m.UpSQL += "\nSELECT 1;" }},
		{"down", func(m *migrations.Migration) {
			m.DownSQL = "DROP TRIGGER membership_commands_no_truncate ON public.membership_commands"
		}},
		{"missing-down", func(m *migrations.Migration) { m.DownSQL = "" }},
		{"version", func(m *migrations.Migration) { m.Version++ }},
		{"name", func(m *migrations.Migration) { m.Name = "other" }},
		{"checksum", func(m *migrations.Migration) { m.Checksum = "other" }},
		{"forged-metadata", func(m *migrations.Migration) { m.Version++; m.Name = "other"; m.Checksum = "other" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			modified := original
			change.edit(&modified)
			if validateOrganizationEvidenceGuard([]migrations.Migration{modified}) == nil {
				t.Fatal("changed protection identity accepted")
			}
		})
	}
	crlf := original
	crlf.UpSQL = strings.ReplaceAll(strings.ReplaceAll(crlf.UpSQL, "\r\n", "\n"), "\n", "\r\n")
	crlf.DownSQL = strings.ReplaceAll(strings.ReplaceAll(crlf.DownSQL, "\r\n", "\n"), "\n", "\r\n")
	if err := validateOrganizationEvidenceGuard([]migrations.Migration{crlf}); err != nil {
		t.Fatal("unchanged legacy line endings rejected", err)
	}
	if err := validateOrganizationEvidenceGuard(nil); err != nil {
		t.Fatal("earlier delivery gained a new dependency", err)
	}
	if validateOrganizationEvidenceGuard([]migrations.Migration{original, original}) == nil {
		t.Fatal("duplicate guard migration accepted")
	}
}

func TestOrganizationEvidenceGuardRejectsBeforeConnection(t *testing.T) {
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	for i := range loaded {
		if loaded[i].Version == organizationEvidenceGuardVersion {
			loaded[i].DownSQL = "SELECT 1"
		}
	}
	for name, call := range map[string]func() error{
		"up":     func() error { return Up(context.Background(), nil, loaded) },
		"down":   func() error { return DownOne(context.Background(), nil, loaded) },
		"verify": func() error { _, err := VerifyApplied(context.Background(), nil, loaded); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); err == nil || !strings.Contains(err.Error(), "published forward-only") {
				t.Fatal("protection package reached database work before validation", err)
			}
		})
	}
}
