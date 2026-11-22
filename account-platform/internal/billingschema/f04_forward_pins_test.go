package billingschema

import (
	"slices"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

func TestF04FrozenForwardMigrationPins(t *testing.T) {
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	pins := []struct {
		version        int64
		name, up, down string
	}{
		{900009, "expired_hold_recovery", "9c926bdb0acbec53cbe3907b92d42430f3177fcd304528b4a4e0a1fe60ae47d5", "f309bf2ac39e67860593585e5db72236b1109b957e859521e839bf2053115fdd"},
		{900010, "metering_deactivation_receipts", "c07c9f04924cb761d6473e614f6ae42e466234359c793b138b62140c8a21f4b8", "9be8bea9e561d6ab85646a1fe3712ac264332cef41f7ac01d36e05131a40aee8"},
	}
	ordered, err := Ordered(loaded)
	if err != nil {
		t.Fatal(err)
	}
	for _, pin := range pins {
		found := false
		for _, item := range ordered {
			if item.Version == pin.version {
				found = true
				if item.Name != pin.name || item.Checksum != pin.up || sqlChecksum(item.UpSQL) != pin.up || sqlChecksum(item.DownSQL) != pin.down {
					t.Fatal("actual migration identity changed", pin.version)
				}
			}
		}
		if !found {
			t.Fatal("actual migration absent", pin.version)
		}
		for _, mutation := range []string{"missing-pair", "missing-up", "missing-down", "changed-up", "changed-up-and-checksum", "changed-down", "metadata-checksum", "metadata-name", "metadata-version"} {
			t.Run(pin.name+"/"+mutation, func(t *testing.T) {
				input := make([]migrations.Migration, 0, len(loaded))
				for _, item := range loaded {
					if item.Version == pin.version {
						switch mutation {
						case "missing-pair":
							continue
						case "missing-up":
							item.UpSQL = ""
						case "missing-down":
							item.DownSQL = ""
						case "changed-up":
							item.UpSQL += "\nSELECT 1;\n"
						case "changed-up-and-checksum":
							item.UpSQL += "\nSELECT 1;\n"
							item.Checksum = sqlChecksum(item.UpSQL)
						case "changed-down":
							item.DownSQL = "SELECT 1;"
						case "metadata-checksum":
							item.Checksum = strings.Repeat("0", 64)
						case "metadata-name":
							item.Name += "_modified"
						case "metadata-version":
							item.Version = 999999
						}
					}
					input = append(input, item)
				}
				if _, err := Ordered(input); err == nil {
					t.Fatal("unfrozen F04 migration accepted", mutation)
				}
			})
		}
	}
	t.Run("canonical-line-endings-preserve-order", func(t *testing.T) {
		input := slices.Clone(loaded)
		for i := range input {
			input[i].UpSQL = strings.ReplaceAll(strings.ReplaceAll(input[i].UpSQL, "\r\n", "\n"), "\n", "\r\n")
			input[i].DownSQL = strings.ReplaceAll(strings.ReplaceAll(input[i].DownSQL, "\r\n", "\n"), "\n", "\r\n")
		}
		result, err := Ordered(input)
		if err != nil || len(result) != len(ordered) {
			t.Fatal("canonical package rejected", err)
		}
		for i := range ordered {
			if result[i].Version != ordered[i].Version {
				t.Fatal("existing migration execution order changed")
			}
		}
	})
	t.Run("earlier-delivery-without-F04-remains-valid", func(t *testing.T) {
		var input []migrations.Migration
		for _, item := range loaded {
			if item.Version != 51 && item.Version != 900008 && item.Version != 900009 && item.Version != 900010 {
				input = append(input, item)
			}
		}
		if _, err := Ordered(input); err != nil {
			t.Fatal("earlier package validation changed", err)
		}
	})
}
