package billinggovernance

import "testing"

func TestReceivableInstalledActivationContract(t *testing.T) {
	for _, state := range []string{"INACTIVE", "ACTIVE"} {
		t.Run(state, func(t *testing.T) {
			s := entitlementFirstDeliverySnapshot()
			s.Activations[6].State = state
			if err := ValidateSnapshot(s); err != nil {
				t.Fatal(err)
			}
			if _, ok := s.Find("RECEIVABLE_WRITE_OFF", 1, 1); !ok {
				t.Fatal("installed contract lost")
			}
		})
	}
	for _, state := range []string{"INACTIVE", "ACTIVE"} {
		t.Run("unknown-version-"+state, func(t *testing.T) {
			s := entitlementFirstDeliverySnapshot()
			s.Activations[6].State = state
			s.Activations[6].PolicyVersion = 2
			if ValidateSnapshot(s) == nil {
				t.Fatal("unknown executable version accepted")
			}
		})
	}
}
