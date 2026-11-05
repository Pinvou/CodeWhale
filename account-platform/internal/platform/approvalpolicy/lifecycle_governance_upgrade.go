package approvalpolicy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

const LifecyclePolicySetUpgradeV1 = "model-lifecycle-force-cancel-v1"
const lifecycleGovernancePolicyDigest = "255c12fe39a639baf24e78ab4812e858feba6cb2fad36067074cb72b7b5ee67c"

func lifecyclePolicyDigest(policies []Policy) string {
	raw, err := json.Marshal(policies)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

// The old complete manifest is independently frozen. Reconstruct only this
// command's historical policy; any unrelated policy drift fails its digest.
func lifecyclePreviousPolicies(policies []Policy) ([]Policy, bool) {
	prior := append([]Policy(nil), policies...)
	found := false
	for i, p := range prior {
		if p.CommandType != "TOKEN_MODEL_LIFECYCLE_CHANGE" {
			continue
		}
		if found || p.PolicyVersion != 3 || p.SnapshotSchemaVersion != 3 || len(p.CriticalResourceFields) == 0 || p.CriticalResourceFields[len(p.CriticalResourceFields)-1] != "force_cancel_inflight" {
			return nil, false
		}
		p.PolicyVersion, p.SnapshotSchemaVersion = 2, 2
		p.CriticalResourceFields = append([]string(nil), p.CriticalResourceFields[:len(p.CriticalResourceFields)-1]...)
		prior[i] = p
		found = true
	}
	return prior, found
}

func CanUpgradeLifecyclePolicySet(from, to string) bool {
	current, err := governancePolicies()
	if err != nil || len(current) != 30 {
		return false
	}
	prior, ok := lifecyclePreviousPolicies(current)
	return ok && from == expandedGovernancePolicyDigest && to == lifecycleGovernancePolicyDigest && lifecyclePolicyDigest(prior) == from && to == lifecyclePolicyDigest(current)
}
