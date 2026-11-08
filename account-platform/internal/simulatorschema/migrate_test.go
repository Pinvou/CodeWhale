package simulatorschema

import (
	"slices"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

func TestSimulatorIdentityRollbackBoundary(t *testing.T) {
	loaded, err := migrations.Load("../../migrations/payment-simulator")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Ordered(loaded); err != nil {
		t.Fatal(err)
	}
	for _, version := range []int64{5, 6} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			changed := slices.Clone(loaded)
			for i := range changed {
				if changed[i].Version == version {
					changed[i].DownSQL = "ALTER TABLE simulator_orders DROP COLUMN merchant_customer_key;"
				}
			}
			if _, err := Ordered(changed); err == nil {
				t.Fatal("edited rollback accepted before CLI mutation")
			}
		})
	}
}
