package billingschema

import (
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
	if j < 1 || versions[j-1] != 29 || j == len(versions)-1 || versions[j+1] != 30 {
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
			changed[i].UpSQL = strings.ReplaceAll(changed[i].UpSQL, "\n", "\r\n")
			changed[i].DownSQL = strings.ReplaceAll(changed[i].DownSQL, "\n", "\r\n")
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
