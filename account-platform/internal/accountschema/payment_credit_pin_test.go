package accountschema

import (
	"fmt"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

func TestPaymentCreditMigrationIdentityAtPublicOrdering(t *testing.T) {
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Ordered(loaded); err != nil {
		t.Fatal("full current migration ordering", err)
	}
	for _, version := range []int64{paymentCreditAdmissionVersion, paymentCreditClosureFenceVersion} {
		index := -1
		for i, migration := range loaded {
			if migration.Version == version {
				index = i
			}
		}
		if index < 0 {
			t.Fatal("required current payment credit migration absent", version)
		}
		for _, mutation := range []string{"up", "down", "rebound-up-checksum", "name", "version", "duplicate"} {
			t.Run(fmt.Sprintf("%d/%s", version, mutation), func(t *testing.T) {
				changed := append([]migrations.Migration(nil), loaded...)
				switch mutation {
				case "up", "rebound-up-checksum":
					changed[index].UpSQL += "\nSELECT 1;"
					if mutation == "rebound-up-checksum" {
						changed[index].Checksum = closureReplaySQLChecksum(changed[index].UpSQL)
					}
				case "down":
					changed[index].DownSQL += "\nSELECT 1;"
				case "name":
					changed[index].Name += "_renamed"
				case "version":
					changed[index].Version = 990000 + version
				case "duplicate":
					changed = append(changed, changed[index])
				}
				if _, err := Ordered(changed); err == nil {
					t.Fatal("public ordering accepted altered payment credit migration")
				}
			})
		}
	}
	// Historical deliveries may predate the optional closure lineage. They
	// still include the ordinary admission schema and acquire the fence when
	// the complete public migration set is subsequently applied.
	var predecessor []migrations.Migration
	for _, migration := range loaded {
		if migration.Version != 27 && migration.Version != 28 && migration.Version != 900005 && migration.Version != paymentCreditClosureFenceVersion {
			predecessor = append(predecessor, migration)
		}
	}
	if _, err := Ordered(predecessor); err != nil {
		t.Fatal("pre-closure public migration ordering", err)
	}
}
