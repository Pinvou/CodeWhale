package approvalconsumer

import "testing"

func TestInstalledBillingConsumptionAndRecoveryFences(t *testing.T) {
	all := All()
	if len(all) != 3 || len(BillingCommands()) != 3 {
		t.Fatal("consumer set expanded")
	}
	for _, d := range all {
		if d.Workload != BillingWorkload || d.StartSource != StartQuiescingHighWatermarkPlusOne || !Match(d, d.CommandType, d.PolicyVersion, d.ExecutorIdentity) || !MatchRoute(LegacyBilling(), d.CommandType, d.PolicyVersion, d.ExecutorIdentity) {
			t.Fatal("registered delivery contract lost")
		}
		forged := d
		forged.PolicyVersion++
		if Match(forged, forged.CommandType, forged.PolicyVersion, forged.ExecutorIdentity) || Match(d, d.CommandType, d.PolicyVersion, "token-command-executor") {
			t.Fatal("forged execution mapping accepted")
		}
		for _, code := range []string{"DOMAIN_REVISION_CHANGED", "DOMAIN_RESOURCE_NOT_FOUND", "DOMAIN_PRECONDITION_REJECTED", "transport_timeout", ""} {
			a := ClassifyProofFailure(d.CommandType, d.PolicyVersion, code)
			if !a.QueryOnly || a.Retryable || a.MaxDomainAttempts != 1 {
				t.Fatal("classification authorized another effect")
			}
		}
	}
	if Match(LegacyBilling(), "WALLET_ADJUSTMENT", 1, BillingWorkload) {
		t.Fatal("shared routing descriptor granted execution")
	}
	if _, ok := LookupDurable("token-command-executor-v1"); ok {
		t.Fatal("future Token executor installed")
	}
	first := all[0]
	all[0].Durable = "tampered"
	if All()[0] != first {
		t.Fatal("registry mutable through returned copy")
	}
}
