package approvalpolicy

import "encoding/json"

// ForCommand preserves the exact lifecycle v2 shape for historical false
// approvals. V3 intent is explicit; the domain must still bind it to the
// immutable persisted plan before approving or executing it.
func ForCommand(commandType string, normalized json.RawMessage) (Policy, error) {
	current, err := Lookup(commandType)
	if err != nil || current.CommandType != "TOKEN_MODEL_LIFECYCLE_CHANGE" {
		return current, err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(normalized, &fields) != nil || fields == nil {
		return Policy{}, ErrPolicyNotRegistered
	}
	raw, present := fields["force_cancel_inflight"]
	if !present {
		return LookupVersion(current.CommandType, 2, 2)
	}
	if string(raw) != "true" && string(raw) != "false" {
		return Policy{}, ErrPolicyNotRegistered
	}
	return LookupVersion(current.CommandType, 3, 3)
}

func ForSnapshot(commandType string, critical json.RawMessage) (Policy, error) {
	current, err := Lookup(commandType)
	if err != nil || current.CommandType != "TOKEN_MODEL_LIFECYCLE_CHANGE" {
		return current, err
	}
	var fields struct {
		PolicyVersion int64 `json:"policy_version"`
	}
	if json.Unmarshal(critical, &fields) != nil {
		return Policy{}, ErrPolicyNotRegistered
	}
	return LookupVersion(current.CommandType, fields.PolicyVersion, int(fields.PolicyVersion))
}
