package billinggovernance

import "testing"

func TestInstalledRefundActivationContract(t *testing.T) {
	for _, state := range []string{"INACTIVE", "ACTIVE"} {
		t.Run(state, func(t *testing.T) {
			s := entitlementFirstDeliverySnapshot()
			for i := range s.Activations {
				if s.Activations[i].CommandType == "REFUND_EXCEPTION_REPAIR" {
					s.Activations[i].State = state
				}
			}
			if e := ValidateSnapshot(s); e != nil {
				t.Fatal(e)
			}
			a, ok := s.Find("REFUND_EXCEPTION_REPAIR", 1, 1)
			if !ok || a.State != state {
				t.Fatal("installed p1/s1 lost")
			}
		})
	}
	for _, state := range []string{"INACTIVE", "ACTIVE"} {
		t.Run("unknown-version-"+state, func(t *testing.T) {
			s := entitlementFirstDeliverySnapshot()
			for i := range s.Activations {
				if s.Activations[i].CommandType == "REFUND_EXCEPTION_REPAIR" {
					s.Activations[i].PolicyVersion = 2
					s.Activations[i].State = state
				}
			}
			if ValidateSnapshot(s) == nil {
				t.Fatal("unknown refund version accepted")
			}
		})
	}
}
