package billingschema

import (
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"testing"
)

func TestRefundActivationCapabilityMigrationBoundary(t *testing.T) {
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range loaded {
		if m.Version == refundActivationLockVersion {
			t.Logf("registered name=%s up=%s actual-up=%s expected-up=%s down=%s expected-down=%s", m.Name, m.Checksum, sqlChecksum(m.UpSQL), refundActivationLockChecksum, sqlChecksum(m.DownSQL), refundActivationLockDownChecksum)
		}
	}
	if _, err := Ordered(loaded); err != nil {
		t.Fatal(err)
	}
	for _, variant := range []string{"missing-capability", "missing-refund-cases", "duplicate", "body", "down", "name"} {
		t.Run(variant, func(t *testing.T) {
			var input []migrations.Migration
			for _, original := range loaded {
				m := original
				if variant == "missing-capability" && m.Version == refundActivationLockVersion || variant == "missing-refund-cases" && m.Version == 67 {
					continue
				}
				if m.Version == refundActivationLockVersion {
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
