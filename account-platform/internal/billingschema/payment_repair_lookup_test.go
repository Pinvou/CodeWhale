package billingschema

import (
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"testing"
)

func TestPaymentRepairLookupMigrationBoundary(t *testing.T) {
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Ordered(loaded); err != nil {
		t.Fatal(err)
	}
	for _, variant := range []string{"body", "down", "name", "duplicate"} {
		t.Run(variant, func(t *testing.T) {
			var input []migrations.Migration
			for _, m := range loaded {
				if m.Version == paymentRepairLookupVersion {
					switch variant {
					case "body":
						m.UpSQL += "\nSELECT 1"
						m.Checksum = sqlChecksum(m.UpSQL)
					case "down":
						m.DownSQL = "DROP INDEX public.payment_order_repairs_order_lookup_idx"
					case "name":
						m.Name = "unregistered"
					case "duplicate":
						input = append(input, m)
					}
				}
				input = append(input, m)
			}
			if _, err := Ordered(input); err == nil {
				t.Fatal("unregistered lookup migration accepted")
			}
		})
	}
}
