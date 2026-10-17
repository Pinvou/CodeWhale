package billingschema

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

func TestRetainedRelayFenceMigrationBoundary(t *testing.T) {
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	ordered, err := Ordered(loaded)
	if err != nil {
		t.Fatal(err)
	}
	var versions []int64
	for _, m := range ordered {
		versions = append(versions, m.Version)
	}
	i := slices.Index(versions, generationVersion)
	if i < 1 || versions[i-1] != 6 || i == len(versions)-1 || versions[i+1] != 7 {
		t.Fatal("relay fence not installed directly after its real lease prerequisite")
	}
	j := slices.Index(versions, intentVersion)
	if j < 1 || versions[j-1] != 29 || j == len(versions)-1 || versions[j+1] != 30 || j+2 >= len(versions) || versions[j+2] != 31 {
		t.Fatal("immutable anomaly intent not installed directly after its real schema prerequisite")
	}
	t.Run("intent-fixed-pins-and-prerequisite", func(t *testing.T) {
		for _, direction := range []string{"up", "down", "prerequisite", "phase"} {
			changed := slices.Clone(loaded)
			var input []migrations.Migration
			for _, m := range changed {
				if direction == "prerequisite" && m.Version == 29 || direction == "phase" && m.Version == intentVersion {
					continue
				}
				if m.Version == intentVersion {
					if direction == "up" {
						m.UpSQL += "\nSELECT 1;\n"
						m.Checksum = sqlChecksum(m.UpSQL)
					}
					if direction == "down" {
						m.DownSQL = "DROP TABLE payment_anomaly_resolution_intents;"
					}
				}
				input = append(input, m)
			}
			if _, err := Ordered(input); err == nil {
				t.Fatal("unfrozen or unpaired anomaly intent migration accepted: " + direction)
			}
		}
	})
	for _, tc := range []struct {
		name   string
		mutate func(*migrations.Migration)
	}{
		{"up-body-change", func(m *migrations.Migration) { m.UpSQL += "\nSELECT 1;\n" }},
		{"up-body-and-checksum-forgery", func(m *migrations.Migration) { m.UpSQL += "\nSELECT 1;\n"; m.Checksum = sqlChecksum(m.UpSQL) }},
		{"down-body-change", func(m *migrations.Migration) {
			m.DownSQL = "ALTER TABLE outbox_events DROP COLUMN publication_generation;"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := slices.Clone(loaded)
			for i := range changed {
				if changed[i].Version == generationVersion {
					tc.mutate(&changed[i])
				}
			}
			if _, err := Ordered(changed); err == nil {
				t.Fatal("edited relay fence accepted before CLI rollback/application")
			}
		})
	}
	t.Run("canonical-line-endings", func(t *testing.T) {
		changed := slices.Clone(loaded)
		for i := range changed {
			changed[i].UpSQL = strings.ReplaceAll(strings.ReplaceAll(changed[i].UpSQL, "\r\n", "\n"), "\n", "\r\n")
			changed[i].DownSQL = strings.ReplaceAll(strings.ReplaceAll(changed[i].DownSQL, "\r\n", "\n"), "\n", "\r\n")
		}
		if _, err := Ordered(changed); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("missing-lease-prerequisite", func(t *testing.T) {
		var changed []migrations.Migration
		for _, m := range loaded {
			if m.Version != 6 {
				changed = append(changed, m)
			}
		}
		if _, err := Ordered(changed); err == nil {
			t.Fatal("relay fence accepted without lease schema")
		}
	})
}

// Ordered is the shared preflight for both public directions. An altered
// first-delivery package must be rejected before either path uses a connection.
func TestRegistrationBonusFrozenMigrationBoundary(t *testing.T) {
	const version int64 = 79
	const upChecksum = "27802b2d3e76e6a42c809d620d3d9833cfaa529d7d5517982318236a56719deb"
	const downChecksum = "56c6319eb573db51e60cb8e8a1a91173b882b4a025c7d6a941f0b01833f027c6"
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	ordered, err := Ordered(loaded)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range ordered {
		if item.Version == version {
			found = true
			if item.Name != "registration_bonus" || item.Checksum != upChecksum || sqlChecksum(item.UpSQL) != upChecksum || sqlChecksum(item.DownSQL) != downChecksum {
				t.Fatal("original registration bonus migration identity changed")
			}
		}
	}
	if !found {
		t.Fatal("original registration bonus migration absent")
	}
	for _, mutation := range []string{"up-body", "up-body-and-checksum", "down-body", "metadata-checksum", "metadata-name", "metadata-version", "missing-up", "missing-down"} {
		t.Run(mutation, func(t *testing.T) {
			input := slices.Clone(loaded)
			for i := range input {
				item := &input[i]
				if item.Version != version {
					continue
				}
				switch mutation {
				case "up-body", "up-body-and-checksum":
					item.UpSQL += "\nSELECT 1;\n"
					if mutation == "up-body-and-checksum" {
						item.Checksum = sqlChecksum(item.UpSQL)
					}
				case "down-body":
					item.DownSQL += "\nSELECT 1;\n"
				case "metadata-checksum":
					item.Checksum = strings.Repeat("0", 64)
				case "metadata-name":
					item.Name += "_modified"
				case "metadata-version":
					item.Version = 999
				case "missing-up":
					item.UpSQL = ""
				case "missing-down":
					item.DownSQL = ""
				}
			}
			if _, err := Ordered(input); err == nil {
				t.Fatal("unfrozen registration bonus package accepted", mutation)
			}
			if err := Up(context.Background(), nil, input); err == nil {
				t.Fatal("edited registration bonus package reached Up", mutation)
			}
			if err := DownOne(context.Background(), nil, input); err == nil {
				t.Fatal("edited registration bonus package reached DownOne", mutation)
			}
		})
	}
	t.Run("canonical-line-endings-preserve-order", func(t *testing.T) {
		input := slices.Clone(loaded)
		for i := range input {
			input[i].UpSQL = strings.ReplaceAll(strings.ReplaceAll(input[i].UpSQL, "\r\n", "\n"), "\n", "\r\n")
			input[i].DownSQL = strings.ReplaceAll(strings.ReplaceAll(input[i].DownSQL, "\r\n", "\n"), "\n", "\r\n")
		}
		actual, err := Ordered(input)
		if err != nil || len(actual) != len(ordered) {
			t.Fatal("canonical original package rejected", err)
		}
		for i := range ordered {
			if actual[i].Version != ordered[i].Version {
				t.Fatal("original execution order changed")
			}
		}
	})
	t.Run("earlier-delivery-without-registration-bonus-remains-valid", func(t *testing.T) {
		var input []migrations.Migration
		for _, item := range loaded {
			if item.Version != version {
				input = append(input, item)
			}
		}
		if _, err := Ordered(input); err != nil {
			t.Fatal("earlier migration package rejected", err)
		}
	})
}
