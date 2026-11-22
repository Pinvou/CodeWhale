package approvalpolicy

// LookupVersion preserves the immutable lifecycle v2 false contract while new
// approvals use v3. Runtime activation remains the strict governance fence.
func LookupVersion(commandType string, policyVersion int64, snapshotVersion int) (Policy, error) {
	policy, err := Lookup(commandType)
	if err != nil {
		return Policy{}, err
	}
	policy.CriticalResourceFields = append([]string(nil), policy.CriticalResourceFields...)
	if policy.PolicyVersion == policyVersion && policy.SnapshotSchemaVersion == snapshotVersion {
		return policy, nil
	}
	if policy.CommandType != "TOKEN_MODEL_LIFECYCLE_CHANGE" || policyVersion != 2 || snapshotVersion != 2 {
		return Policy{}, ErrPolicyNotRegistered
	}
	policy.PolicyVersion, policy.SnapshotSchemaVersion = 2, 2
	policy.CriticalResourceFields = policy.CriticalResourceFields[:len(policy.CriticalResourceFields)-1]
	return policy, nil
}
