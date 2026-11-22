package billingschema

import (
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"testing"
)

func TestLedgerActivationCapabilityMigrationBoundary(t *testing.T) {
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Ordered(loaded); err != nil {
		t.Fatal(err)
	}
	for _, variant := range []string{"missing-capability", "missing-ledger-cases", "duplicate", "body", "down", "name"} {
		t.Run(variant, func(t *testing.T) {
			var input []migrations.Migration
			for _, original := range loaded {
				m := original
				if variant == "missing-capability" && m.Version == ledgerActivationLockVersion || variant == "missing-ledger-cases" && m.Version == 61 {
					continue
				}
				if m.Version == ledgerActivationLockVersion {
					switch variant {
					case "body":
						m.UpSQL += "\nSELECT 1;"
						m.Checksum = sqlChecksum(m.UpSQL)
					case "down":
						m.DownSQL = "DROP SCHEMA billing_approval_execution CASCADE;"
					case "name":
						m.Name = "unregistered_activation_capability"
					case "duplicate":
						input = append(input, m)
					}
				}
				input = append(input, m)
			}
			if _, err := Ordered(input); err == nil {
				t.Fatal("unregistered or unpaired capability migration accepted")
			}
		})
	}
}
