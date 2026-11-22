package token

import "encoding/json"

// Missing flags are only the frozen v2 false contract. New plans use v3;
// existing scheduled plans retain the shape actually approved by Account.
func lifecyclePlanApprovalFlag(plan LifecyclePlan) *bool {
	var frozen struct {
		PolicyVersion int64 `json:"policy_version"`
	}
	if len(plan.CriticalResourceVersions) > 0 && json.Unmarshal(plan.CriticalResourceVersions, &frozen) == nil && frozen.PolicyVersion == 2 {
		return nil
	}
	flag := plan.ForceCancelInflight
	return &flag
}

func lifecyclePlanFlagMatches(plan LifecyclePlan, flag *bool) bool {
	if flag == nil {
		return !plan.ForceCancelInflight
	}
	return *flag == plan.ForceCancelInflight && (!*flag || plan.TargetLifecycle == "REMOVED")
}

func lifecycleProtocolFlagsMatch(command, critical *bool, version int64, target string) bool {
	if version == 2 {
		return command == nil && critical == nil
	}
	return version == 3 && command != nil && critical != nil && *command == *critical && (!*command || target == "REMOVED")
}
