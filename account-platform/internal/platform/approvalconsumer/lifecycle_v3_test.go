package approvalconsumer

import "testing"

func TestLifecycleV3UsesExistingPhysicalDurableWithExactHistoricalDispatch(t *testing.T) {
	descriptor, ok := LookupDurable("token-model-lifecycle-executor-v1")
	if !ok || descriptor.PolicyVersion != 2 {
		t.Fatal("historical durable identity changed")
	}
	for _, version := range []int64{2, 3} {
		if !Match(descriptor, descriptor.CommandType, version, TokenWorkload) || !MatchRoute(descriptor, descriptor.CommandType, version, TokenWorkload) {
			t.Fatal("published lifecycle contract unavailable", version)
		}
	}
	for _, version := range []int64{0, 1, 4} {
		if Match(descriptor, descriptor.CommandType, version, TokenWorkload) {
			t.Fatal("unpublished version accepted", version)
		}
	}
	for _, d := range All() {
		if d.CommandType != descriptor.CommandType && Match(d, d.CommandType, d.PolicyVersion+1, d.ExecutorIdentity) {
			t.Fatal("other command version relaxed", d.CommandType)
		}
	}
	forged := descriptor
	forged.PolicyVersion = 3
	if Match(forged, descriptor.CommandType, 3, TokenWorkload) || Match(descriptor, descriptor.CommandType, 3, BillingWorkload) || MatchRoute(LegacyBilling(), descriptor.CommandType, 3, TokenWorkload) {
		t.Fatal("forged descriptor or workload accepted")
	}
}
