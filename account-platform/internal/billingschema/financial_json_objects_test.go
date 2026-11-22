package billingschema

import (
	"context"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"strings"
	"testing"
)

func TestFinancialJSONObjectPins(t *testing.T) {
	all, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	var original migrations.Migration
	for _, m := range all {
		if m.Version == financialJSONObjectVersion {
			original = m
		}
	}
	if original.Version == 0 || validateFinancialJSONObjects([]migrations.Migration{original}) != nil {
		t.Fatal("official guard absent")
	}
	for _, tc := range []struct {
		name string
		edit func(*migrations.Migration)
	}{
		{"up", func(m *migrations.Migration) { m.UpSQL += "SELECT 1;" }},
		{"down", func(m *migrations.Migration) { m.DownSQL = "SELECT 1" }},
		{"version", func(m *migrations.Migration) { m.Version++ }},
		{"name", func(m *migrations.Migration) { m.Name = "other" }},
		{"checksum", func(m *migrations.Migration) { m.Checksum = "other" }},
		{"renamed-tampered-down", func(m *migrations.Migration) {
			m.Version++
			m.Name = "other"
			m.Checksum = "other"
			m.DownSQL = "SELECT 1"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := original
			tc.edit(&m)
			if validateFinancialJSONObjects([]migrations.Migration{m}) == nil {
				t.Fatal("guard identity changed")
			}
		})
	}
	if validateFinancialJSONObjects([]migrations.Migration{original, original}) == nil {
		t.Fatal("duplicate accepted")
	}
	if err = validateFinancialJSONObjects(nil); err != nil {
		t.Fatal("old package gained dependency")
	}
	original.UpSQL = strings.ReplaceAll(original.UpSQL, "\n", "\r\n")
	original.DownSQL = strings.ReplaceAll(original.DownSQL, "\n", "\r\n")
	if err = validateFinancialJSONObjects([]migrations.Migration{original}); err != nil {
		t.Fatal(err)
	}
}
func TestFinancialJSONObjectRejectsBeforeConnection(t *testing.T) {
	all, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	for i := range all {
		if all[i].Version == financialJSONObjectVersion {
			all[i].DownSQL = "SELECT 1"
		}
	}
	for name, fn := range map[string]func() error{"up": func() error { return Up(context.Background(), nil, all) }, "down": func() error { return DownOne(context.Background(), nil, all) }, "verify": func() error { _, e := VerifyApplied(context.Background(), nil, all); return e }} {
		t.Run(name, func(t *testing.T) {
			e := fn()
			if e == nil || !strings.Contains(e.Error(), "published forward-only") {
				t.Fatal("tampered protection reached database", e)
			}
		})
	}
}
