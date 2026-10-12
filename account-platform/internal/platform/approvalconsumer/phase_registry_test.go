package approvalconsumer

import "testing"

func TestInstalledBillingConsumptionAndRecoveryFences(t *testing.T) {
	want := map[string]Descriptor{
		"RECEIVABLE_WRITE_OFF":             {CommandType: "RECEIVABLE_WRITE_OFF", PolicyVersion: 1, ExecutorIdentity: BillingWorkload, Durable: "billing-receivable-write-off-executor-p1-v1", Workload: BillingWorkload, StartSource: StartQuiescingHighWatermarkPlusOne},
		"ENTITLEMENT_COMPONENT_ADJUSTMENT": {CommandType: "ENTITLEMENT_COMPONENT_ADJUSTMENT", PolicyVersion: 2, ExecutorIdentity: BillingWorkload, Durable: "billing-entitlement-component-adjustment-executor-p2-v1", Workload: BillingWorkload, StartSource: StartQuiescingHighWatermarkPlusOne},
		"LEDGER_REPAIR":                    {CommandType: "LEDGER_REPAIR", PolicyVersion: 1, ExecutorIdentity: BillingWorkload, Durable: "billing-ledger-repair-executor-p1-v1", Workload: BillingWorkload, StartSource: StartQuiescingHighWatermarkPlusOne},
		"WALLET_ADJUSTMENT":                {CommandType: "WALLET_ADJUSTMENT", PolicyVersion: 1, ExecutorIdentity: BillingWorkload, Durable: "billing-wallet-adjustment-executor-p1-v1", Workload: BillingWorkload, StartSource: StartQuiescingHighWatermarkPlusOne},
		"PAYMENT_PROVIDER_RESUME":          {CommandType: "PAYMENT_PROVIDER_RESUME", PolicyVersion: 2, ExecutorIdentity: BillingWorkload, Durable: "billing-payment-provider-resume-executor-p2-v1", Workload: BillingWorkload, StartSource: StartQuiescingHighWatermarkPlusOne},
		"PAYMENT_PRODUCT_RESUME":           {CommandType: "PAYMENT_PRODUCT_RESUME", PolicyVersion: 1, ExecutorIdentity: BillingWorkload, Durable: "billing-payment-product-resume-executor-p1-v1", Workload: BillingWorkload, StartSource: StartQuiescingHighWatermarkPlusOne},
		"TOKEN_MODEL_LIFECYCLE_CHANGE":     {CommandType: "TOKEN_MODEL_LIFECYCLE_CHANGE", PolicyVersion: 2, ExecutorIdentity: TokenWorkload, Durable: "token-model-lifecycle-executor-v1", Workload: TokenWorkload, StartSource: StartExisting},
		"TOKEN_MODEL_CATALOG_ROLLBACK":     {CommandType: "TOKEN_MODEL_CATALOG_ROLLBACK", PolicyVersion: 2, ExecutorIdentity: TokenWorkload, Durable: "token-catalog-rollback-executor-v1", Workload: TokenWorkload, StartSource: StartExisting},
		"TOKEN_CREDENTIAL_REVOKE":          {CommandType: "TOKEN_CREDENTIAL_REVOKE", PolicyVersion: 1, ExecutorIdentity: TokenWorkload, Durable: "token-credential-revoke-executor-v1", Workload: TokenWorkload, StartSource: StartExisting},
	}
	all := All()
	if len(all) != len(want) || len(BillingCommands()) != 6 || len(TokenCommands()) != 3 {
		t.Fatal("installed consumer set differs")
	}
	seen := make(map[string]bool)
	for _, d := range all {
		if expected, ok := want[d.CommandType]; !ok || expected != d || seen[d.CommandType] {
			t.Fatalf("delivery contract differs: %s", d.CommandType)
		}
		seen[d.CommandType] = true
		if !Match(d, d.CommandType, d.PolicyVersion, d.ExecutorIdentity) || !MatchRoute(d, d.CommandType, d.PolicyVersion, d.ExecutorIdentity) {
			t.Fatal("registered delivery contract lost")
		}
		otherWorkload := TokenWorkload
		if d.Workload == BillingWorkload {
			if MatchRoute(LegacyBilling(), d.CommandType, d.PolicyVersion, d.ExecutorIdentity) != (d.CommandType != "ENTITLEMENT_COMPONENT_ADJUSTMENT") {
				t.Fatal("legacy Billing routing lost")
			}
		} else {
			otherWorkload = BillingWorkload
			if MatchRoute(LegacyBilling(), d.CommandType, d.PolicyVersion, d.ExecutorIdentity) {
				t.Fatal("legacy Billing route accepted Token authority")
			}
		}
		forged := d
		forged.PolicyVersion++
		if Match(forged, forged.CommandType, forged.PolicyVersion, forged.ExecutorIdentity) || Match(d, d.CommandType, d.PolicyVersion, otherWorkload) {
			t.Fatal("forged execution mapping accepted")
		}
		for _, code := range []string{"DOMAIN_REVISION_CHANGED", "DOMAIN_RESOURCE_NOT_FOUND", "DOMAIN_PRECONDITION_REJECTED", "transport_timeout", ""} {
			assessment := ClassifyProofFailure(d.CommandType, d.PolicyVersion, code)
			if !assessment.QueryOnly || assessment.Retryable || assessment.MaxDomainAttempts != 1 {
				t.Fatal("classification authorized another effect")
			}
		}
	}
	for _, group := range []struct {
		items    []Descriptor
		workload string
	}{{BillingCommands(), BillingWorkload}, {TokenCommands(), TokenWorkload}} {
		for _, d := range group.items {
			if d != want[d.CommandType] || d.Workload != group.workload {
				t.Fatal("workload subset differs")
			}
		}
	}
	if Match(LegacyBilling(), "WALLET_ADJUSTMENT", 1, BillingWorkload) {
		t.Fatal("shared routing descriptor granted execution")
	}
	for _, durable := range []string{"token-command-executor-v1", "token-usage-reconciliation-executor-v1", "token-organization-executor-v1"} {
		if _, ok := LookupDurable(durable); ok {
			t.Fatalf("undelivered executor installed: %s", durable)
		}
	}
	for _, d := range All() {
		if d.CommandType == "TOKEN_USAGE_RECONCILIATION_RETRY" {
			t.Fatal("AuditOnly usage retry must not gain an approval consumer")
		}
	}
	first := all[0]
	all[0].Durable = "tampered"
	if All()[0] != first {
		t.Fatal("registry mutable through returned copy")
	}
}
