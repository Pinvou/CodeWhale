package billinggovernance

import (
	"slices"
	"testing"
	"time"
)

func entitlementFirstDeliverySnapshot() Snapshot {
	s := Snapshot{SchemaVersion: 1}
	commands := []string{"CHARGEBACK_FINANCIAL_REPAIR", "ENTITLEMENT_COMPONENT_ADJUSTMENT", "LEDGER_REPAIR", "PAYMENT_FINANCIAL_REPAIR", "PAYMENT_PRODUCT_RESUME", "PAYMENT_PROVIDER_RESUME", "RECEIVABLE_WRITE_OFF", "RECONCILIATION_FINANCIAL_REPAIR", "REFUND_EXCEPTION_REPAIR", "USAGE_SETTLEMENT_REPAIR", "WALLET_ADJUSTMENT"}
	for _, command := range commands {
		policy := int64(1)
		if command == "PAYMENT_PROVIDER_RESUME" {
			policy = 2
		}
		s.Activations = append(s.Activations, Activation{CommandType: command, PolicyVersion: policy, SnapshotSchemaVersion: 1, State: "INACTIVE", Version: 1, UpdatedAt: time.Now().UTC()})
	}
	return s
}

func TestEntitlementFirstDeliveryActivationContract(t *testing.T) {
	t.Run("immutable-v1-inactive", func(t *testing.T) {
		if err := ValidateSnapshot(entitlementFirstDeliverySnapshot()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("version2-inactive-before-operator", func(t *testing.T) {
		s := entitlementFirstDeliverySnapshot()
		current := s.Activations[1]
		current.PolicyVersion, current.SnapshotSchemaVersion = 2, 2
		s.Activations = slices.Insert(s.Activations, 2, current)
		if err := ValidateSnapshot(s); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("version2-active-after-operator", func(t *testing.T) {
		s := entitlementFirstDeliverySnapshot()
		current := s.Activations[1]
		current.PolicyVersion, current.SnapshotSchemaVersion, current.State = 2, 2, "ACTIVE"
		s.Activations = slices.Insert(s.Activations, 2, current)
		if err := ValidateSnapshot(s); err != nil {
			t.Fatal(err)
		}
		if historical, ok := s.Find("ENTITLEMENT_COMPONENT_ADJUSTMENT", 1, 1); !ok || historical.State != "INACTIVE" {
			t.Fatal("original inactive v1 history was lost")
		}
	})
	for _, tc := range []struct {
		name   string
		policy int64
		schema int
		state  string
	}{
		{"old-policy-active", 1, 1, "ACTIVE"}, {"mixed-pair", 2, 1, "INACTIVE"}, {"unknown-version", 3, 2, "INACTIVE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := entitlementFirstDeliverySnapshot()
			s.Activations[1].PolicyVersion = tc.policy
			s.Activations[1].SnapshotSchemaVersion = tc.schema
			s.Activations[1].State = tc.state
			if ValidateSnapshot(s) == nil {
				t.Fatal("accepted unknown or obsolete executable policy")
			}
		})
	}
	t.Run("later-command-stays-inactive", func(t *testing.T) {
		s := entitlementFirstDeliverySnapshot()
		s.Activations[7].State = "ACTIVE"
		if ValidateSnapshot(s) == nil {
			t.Fatal("advertised undelivered reconciliation repair")
		}
	})
}
