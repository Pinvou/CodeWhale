package billingschema

import (
	"slices"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

func TestLatePaymentQueryRegisteredIdentity(t *testing.T) {
	input, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Ordered(input); err != nil {
		t.Fatal(err)
	}
	at := slices.IndexFunc(input, func(m migrations.Migration) bool { return m.Version == 900030 })
	if at < 0 {
		t.Fatal("missing Query dispatch identity migration")
	}
	for _, kind := range []string{"version", "name", "checksum", "up", "down"} {
		t.Run(kind, func(t *testing.T) {
			changed := slices.Clone(input)
			switch kind {
			case "version":
				changed[at].Version++
			case "name":
				changed[at].Name += "_changed"
			case "checksum":
				changed[at].Checksum = "changed"
			case "up":
				changed[at].UpSQL += "\nSELECT 1;\n"
			case "down":
				changed[at].DownSQL = "SELECT 1;\n"
			}
			if _, err := Ordered(changed); err == nil {
				t.Fatal("Query migration identity accepted mutation", kind)
			}
		})
	}
}
