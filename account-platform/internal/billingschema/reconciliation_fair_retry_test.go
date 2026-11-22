package billingschema

import (
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"slices"
	"testing"
)

func TestReconciliationFairRetryRegisteredIdentity(t *testing.T) {
	input, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Ordered(input); err != nil {
		t.Fatal(err)
	}
	at := slices.IndexFunc(input, func(m migrations.Migration) bool { return m.Version == 900026 })
	if at < 0 {
		t.Fatal("missing registered forward migration")
	}
	for _, kind := range []string{"version", "name", "checksum", "up", "down"} {
		t.Run(kind, func(t *testing.T) {
			copy := slices.Clone(input)
			switch kind {
			case "version":
				copy[at].Version++
			case "name":
				copy[at].Name += "_changed"
			case "checksum":
				copy[at].Checksum = "changed"
			case "up":
				copy[at].UpSQL += "\nSELECT 1;\n"
			case "down":
				copy[at].DownSQL = "SELECT 1;\n"
			}
			if _, err := Ordered(copy); err == nil {
				t.Fatal("changed registered retry identity accepted")
			}
		})
	}
}
