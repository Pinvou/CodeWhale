package approvalpolicy

import (
	"reflect"
	"testing"
)

func TestLifecycleV3SelectionDoesNotActivatePolicy(t *testing.T) {
	before, _ := PolicySetDigest()
	current, _ := Lookup("TOKEN_MODEL_LIFECYCLE_CHANGE")
	selected, err := LookupVersion("TOKEN_MODEL_LIFECYCLE_CHANGE", 3, 3)
	if err != nil || selected.PolicyVersion != 3 || selected.SnapshotSchemaVersion != 3 || len(selected.CriticalResourceFields) != len(current.CriticalResourceFields) {
		t.Fatal("explicit v3 unavailable")
	}
	if selected.Mode != current.Mode || selected.RequesterPermission != current.RequesterPermission || selected.ApproverPermission != current.ApproverPermission || selected.ExpiresAfter != current.ExpiresAfter || selected.ExecutionWindow != current.ExecutionWindow || selected.ApprovalCount != current.ApprovalCount || selected.SelfApprovalForbidden != current.SelfApprovalForbidden {
		t.Fatal("existing approval constraints changed")
	}
	selected.CriticalResourceFields[0] = "untrusted-local-edit"
	after, _ := PolicySetDigest()
	again, _ := Lookup("TOKEN_MODEL_LIFECYCLE_CHANGE")
	if before != after || !reflect.DeepEqual(current, again) || current.PolicyVersion != 3 || current.SnapshotSchemaVersion != 3 {
		t.Fatal("v3 selection mutated active manifest")
	}
	historical, err := LookupVersion("TOKEN_MODEL_LIFECYCLE_CHANGE", 2, 2)
	if err != nil || historical.PolicyVersion != 2 || historical.SnapshotSchemaVersion != 2 || len(historical.CriticalResourceFields) != len(current.CriticalResourceFields)-1 {
		t.Fatal("v2 selection changed")
	}
	for _, v := range [][2]int{{3, 2}, {2, 3}, {4, 4}} {
		if _, err := LookupVersion(current.CommandType, int64(v[0]), v[1]); err == nil {
			t.Fatal("unsupported pair accepted")
		}
	}
	for _, p := range All() {
		if p.CommandType != current.CommandType {
			if _, err := LookupVersion(p.CommandType, 3, 3); err == nil && (p.PolicyVersion != 3 || p.SnapshotSchemaVersion != 3) {
				t.Fatal("other policy version relaxed")
			}
		}
	}
	if !CanUpgradeLifecyclePolicySet(expandedGovernancePolicyDigest, before) || !CanExpandGovernancePolicySet(legacyGovernancePolicyDigest, expandedGovernancePolicyDigest) {
		t.Fatal("original governance evidence changed")
	}
	t.Logf("unchanged default policy-set digest: %s", before)
}
