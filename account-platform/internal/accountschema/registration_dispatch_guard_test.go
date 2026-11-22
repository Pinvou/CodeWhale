package accountschema

import (
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"slices"
	"testing"
)

func TestRegistrationDispatchMigrationFrozenIdentity(t *testing.T) {
	installed, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Ordered(installed); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"name", "version", "up", "down", "checksum", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			changed := slices.Clone(installed)
			for i := range changed {
				if changed[i].Version != registrationDispatchVersion {
					continue
				}
				switch mode {
				case "name":
					changed[i].Name = "wrong_identity_claims"
				case "version":
					changed[i].Version = 990008
				case "up":
					changed[i].UpSQL += "\nSELECT 1;"
				case "down":
					changed[i].DownSQL += "\nSELECT 1;"
				case "checksum":
					changed[i].Checksum = "altered"
				case "duplicate":
					changed = append(changed, changed[i])
				}
				break
			}
			if _, err := Ordered(changed); err == nil {
				t.Fatal("altered identity migration accepted")
			}
		})
	}
}
