package billingschema

import (
	"slices"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

// The public rollback/application entry point must reject edited receipts before
// opening a transaction, including a forged checksum supplied by the caller.
func TestPricingReceiptMigrationPreservesFrozenFirstDeliveryBoundary(t *testing.T) {
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
	index := slices.Index(versions, pricingReceiptVersion)
	if index < 1 || versions[index-1] != 51 {
		t.Fatal("receipt prerequisite must be installed through public ordering")
	}
	for _, kind := range []string{"up", "forged-up", "down", "missing-receipt", "missing-pricing", "missing-foundation"} {
		t.Run(kind, func(t *testing.T) {
			var changed []migrations.Migration
			for _, m := range loaded {
				if kind == "missing-receipt" && m.Version == pricingReceiptVersion || kind == "missing-pricing" && m.Version == 51 || kind == "missing-foundation" && m.Version == 22 {
					continue
				}
				if m.Version == pricingReceiptVersion {
					if kind == "up" || kind == "forged-up" {
						m.UpSQL += "\nSELECT 1;\n"
					}
					if kind == "forged-up" {
						m.Checksum = sqlChecksum(m.UpSQL)
					}
					if kind == "down" {
						m.DownSQL = "DROP TABLE pricing_operation_commands;"
					}
				}
				changed = append(changed, m)
			}
			if _, err := Ordered(changed); err == nil {
				t.Fatal("altered pricing receipt delivery accepted: " + kind)
			}
		})
	}
}
