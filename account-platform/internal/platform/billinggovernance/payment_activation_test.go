package billinggovernance

import "testing"

func TestInstalledPaymentActivationContract(t *testing.T) {
	for _, state := range []string{"INACTIVE", "ACTIVE"} {
		t.Run(state, func(t *testing.T) {
			s := entitlementFirstDeliverySnapshot()
			for i := range s.Activations {
				if s.Activations[i].CommandType == "PAYMENT_FINANCIAL_REPAIR" {
					s.Activations[i].State = state
				}
			}
			if e := ValidateSnapshot(s); e != nil {
				t.Fatal(e)
			}
			a, ok := s.Find("PAYMENT_FINANCIAL_REPAIR", 1, 1)
			if !ok || a.State != state {
				t.Fatal("installed p1/s1 lost")
			}
		})
	}
	for _, command := range []string{"UNKNOWN_FINANCIAL_REPAIR"} {
		t.Run("future-"+command, func(t *testing.T) {
			s := entitlementFirstDeliverySnapshot()
			forged := s.Activations[0]
			forged.CommandType = command
			forged.State = "ACTIVE"
			s.Activations = append(s.Activations, forged)
			if ValidateSnapshot(s) == nil {
				t.Fatal("future command became executable")
			}
		})
	}
}
