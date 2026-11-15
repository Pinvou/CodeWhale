package billingschema

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

func TestHistoricalStateRollbackIdentityRejectsChangedSQL(t *testing.T) {
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Ordered(loaded); err != nil {
		t.Fatal("original published migration package rejected", err)
	}
	for _, version := range []int64{10, 14, 27, 49} {
		for _, field := range []string{"up-body-and-checksum", "down-body", "name", "version"} {
			t.Run(strconv.FormatInt(version, 10)+"/"+field, func(t *testing.T) {
				changed := slices.Clone(loaded)
				found := false
				for index := range changed {
					migration := &changed[index]
					if migration.Version != version {
						continue
					}
					found = true
					switch field {
					case "up-body-and-checksum":
						migration.UpSQL += "\nSELECT 1;\n"
						migration.Checksum = sqlChecksum(migration.UpSQL)
					case "down-body":
						migration.DownSQL += "\nSELECT 1;\n"
					case "name":
						migration.Name = "different_history"
					case "version":
						migration.Version += 1000000
					}
				}
				if !found {
					t.Fatal("original historical migration missing", version)
				}
				if _, err := Ordered(changed); err == nil || !strings.Contains(err.Error(), "historical state rollback requires the exact original migration identity") {
					t.Fatalf("changed protected identity was not specifically rejected: %v", err)
				}
			})
		}
	}
}
