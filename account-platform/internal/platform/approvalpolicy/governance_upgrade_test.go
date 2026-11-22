package approvalpolicy

import "testing"

func TestGovernanceExpansionBindsBothCompleteManifests(t *testing.T) {
	policies, err := governancePolicies()
	if err != nil || !CanExpandGovernancePolicySet(legacyGovernancePolicyDigest, expandedGovernancePolicyDigest) {
		t.Fatal("published expansion no longer matches immutable policy contracts")
	}
	for _, pair := range [][2]string{{expandedGovernancePolicyDigest, legacyGovernancePolicyDigest}, {"unknown", expandedGovernancePolicyDigest}, {legacyGovernancePolicyDigest, "unknown"}, {expandedGovernancePolicyDigest, expandedGovernancePolicyDigest}} {
		if CanExpandGovernancePolicySet(pair[0], pair[1]) {
			t.Fatal("unknown, reversed or repeated expansion accepted")
		}
	}
	changed := append([]Policy(nil), policies...)
	changed[0].PolicyVersion++
	if canExpandGovernancePolicySet(legacyGovernancePolicyDigest, expandedGovernancePolicyDigest, changed) ||
		canExpandGovernancePolicySet(legacyGovernancePolicyDigest, expandedGovernancePolicyDigest, policies[:len(policies)-1]) {
		t.Fatal("changed policy or missing command accepted")
	}
}
