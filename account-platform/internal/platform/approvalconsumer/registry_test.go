package approvalconsumer

import "testing"

func TestApprovalConsumerRegistryContract(t *testing.T) {
	all := All()
	if len(all) != 7 || len(BillingCommands()) != 4 || len(TokenCommands()) != 3 {
		t.Fatalf("unexpected registry sizes: all=%d billing=%d token=%d", len(all), len(BillingCommands()), len(TokenCommands()))
	}
	seenCommands, seenDurables := map[string]bool{}, map[string]bool{}
	for _, descriptor := range all {
		if descriptor.CommandType == "" || descriptor.PolicyVersion < 1 || descriptor.ExecutorIdentity != descriptor.Workload ||
			descriptor.Durable == "" || seenCommands[descriptor.CommandType] || seenDurables[descriptor.Durable] {
			t.Fatalf("invalid or duplicate descriptor: %+v", descriptor)
		}
		seenCommands[descriptor.CommandType], seenDurables[descriptor.Durable] = true, true
		lookup, ok := LookupDurable(descriptor.Durable)
		if !ok || lookup != descriptor || !Match(lookup, descriptor.CommandType, descriptor.PolicyVersion, descriptor.ExecutorIdentity) {
			t.Fatalf("descriptor does not round trip: %+v", descriptor)
		}
		if descriptor.Workload == BillingWorkload && descriptor.StartSource != StartQuiescingHighWatermarkPlusOne {
			t.Fatalf("billing durable does not require Q+1: %+v", descriptor)
		}
		if descriptor.Workload == TokenWorkload && descriptor.StartSource != StartExisting {
			t.Fatalf("token durable changed its start source: %+v", descriptor)
		}
	}
	legacy := LegacyBilling()
	if legacy.CommandType != "" || legacy.PolicyVersion != 0 || legacy.Durable == "" || legacy.StartSource != StartLegacySharedPreFenceCompatibility {
		t.Fatalf("legacy billing must remain a compatibility descriptor: %+v", legacy)
	}
	if Match(legacy, "WALLET_ADJUSTMENT", 1, BillingWorkload) {
		t.Fatal("legacy shared durable must not become an eighth command mapping")
	}
	if _, ok := LookupDurable("unknown"); ok {
		t.Fatal("unknown durable accepted")
	}
	descriptor := all[0]
	if _, ok := LookupDurable(" " + descriptor.Durable); ok {
		t.Fatal("noncanonical durable accepted")
	}
	if Match(descriptor, " "+descriptor.CommandType, descriptor.PolicyVersion, descriptor.ExecutorIdentity) ||
		Match(descriptor, descriptor.CommandType, descriptor.PolicyVersion, descriptor.ExecutorIdentity+" ") {
		t.Fatal("noncanonical command binding accepted")
	}
	forged := descriptor
	forged.PolicyVersion++
	if Match(forged, forged.CommandType, forged.PolicyVersion, forged.ExecutorIdentity) {
		t.Fatal("caller-defined registry descriptor accepted")
	}
	all[0].Durable = "mutated"
	if All()[0] != descriptor {
		t.Fatal("caller mutated registry")
	}
}
