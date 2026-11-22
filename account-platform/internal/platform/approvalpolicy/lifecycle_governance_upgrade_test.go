package approvalpolicy

import "testing"

func TestLifecycleUpgradeBindsExactPublishedManifests(t *testing.T) {
	if !CanUpgradeLifecyclePolicySet(expandedGovernancePolicyDigest, lifecycleGovernancePolicyDigest) {
		t.Fatal("exact transition unavailable")
	}
	for _, pair := range [][2]string{{legacyGovernancePolicyDigest, lifecycleGovernancePolicyDigest}, {lifecycleGovernancePolicyDigest, lifecycleGovernancePolicyDigest}, {lifecycleGovernancePolicyDigest, expandedGovernancePolicyDigest}, {expandedGovernancePolicyDigest, "unknown"}} {
		if CanUpgradeLifecyclePolicySet(pair[0], pair[1]) {
			t.Fatal("unregistered transition accepted")
		}
	}
	original := policies["TOKEN_ROUTE_UPSERT"]
	changed := original
	changed.RequesterPermission = "unexpected.permission"
	policies[changed.CommandType] = changed
	defer func() { policies[original.CommandType] = original }()
	if CanUpgradeLifecyclePolicySet(expandedGovernancePolicyDigest, lifecycleGovernancePolicyDigest) {
		t.Fatal("unrelated policy drift accepted")
	}
}
