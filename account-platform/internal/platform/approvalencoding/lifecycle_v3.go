package approvalencoding

import "time"

// NormalizeLifecycleV3DomainCommand freezes a new domain command in the same
// time representation as its proof. It must run before storing both intent
// fingerprints and commands; exact lease comparisons then remain unchanged.
func NormalizeLifecycleV3DomainCommand(raw []byte) ([]byte, error) {
	value, err := parseDocument(raw)
	if err != nil {
		return nil, err
	}
	spec, _ := commandSpecForVersion("TOKEN_MODEL_LIFECYCLE_CHANGE", 3)
	object, err := requireObject(value, spec.normalizedKeys...)
	if err != nil {
		return nil, err
	}
	if object["force_cancel_inflight"].kind != jsonBool {
		return nil, ErrInvalidDocument
	}
	if err := normalizeLifecycleV3TimeFields(object, "effective_at", "existing_session_cutoff_at"); err != nil {
		return nil, err
	}
	return canonicalJSON(value)
}

func normalizeLifecycleV3TimeFields(object map[string]*jsonValue, fields ...string) error {
	for _, field := range fields {
		value, ok := object[field]
		if !ok {
			return ErrInvalidDocument
		}
		if value.kind == jsonNull && field == "existing_session_cutoff_at" {
			continue
		}
		text, err := stringValue(value)
		if err != nil {
			return ErrInvalidDocument
		}
		instant, err := time.Parse(time.RFC3339Nano, text)
		if err != nil || instant.IsZero() {
			return ErrInvalidDocument
		}
		*value = *jsonString(instant.UTC().Format(time.RFC3339Nano))
	}
	return nil
}

// Persisted domain timestamps can carry the pgx connection's local offset.
// Only this version's registered time fields cross that adapter boundary;
// canonical proof/event decoders still require exact UTC RFC3339Nano strings.
// Historical v2 input and every frozen payload remain byte-for-byte unchanged.
func normalizeLifecycleV3DomainTimes(normalized, critical, financial *jsonValue) error {
	for _, part := range []struct {
		value  *jsonValue
		fields []string
	}{{normalized, []string{"effective_at", "existing_session_cutoff_at"}}, {critical, []string{"effective_at", "existing_session_cutoff_at"}}, {financial, []string{"billing_as_of"}}} {
		object, err := objectValue(part.value)
		if err != nil {
			return err
		}
		if err := normalizeLifecycleV3TimeFields(object, part.fields...); err != nil {
			return err
		}
	}
	return nil
}

// commandSpecForVersion selects only published contracts and the explicit
// lifecycle v3 prototype. The default commandSpecs map remains unchanged.
// Merely supporting these bytes does not activate a governance policy.
func commandSpecForVersion(commandType string, policyVersion int64) (commandSpec, bool) {
	spec, ok := commandSpecs[commandType]
	if !ok {
		return commandSpec{}, false
	}
	if spec.policyVersion == policyVersion {
		return spec, true
	}
	if commandType != "TOKEN_MODEL_LIFECYCLE_CHANGE" || policyVersion != 3 {
		return commandSpec{}, false
	}
	spec.policyVersion, spec.snapshotVersion = 3, 3
	spec.normalizedKeys = append(append([]string(nil), spec.normalizedKeys...), "force_cancel_inflight")
	spec.criticalKeys = append(append([]string(nil), spec.criticalKeys...), "force_cancel_inflight")
	return spec, true
}

func validateLifecycleV3(targetID string, n, c, f map[string]*jsonValue, ints map[string]int64) error {
	if !validUUID(targetID) || !equalString(n["model_id"], targetID) || !equalString(c["model_id"], targetID) ||
		!validUUIDField(n["lifecycle_plan_id"]) || ints["n.lifecycle_plan_version"] < 1 || ints["n.lifecycle_plan_version"] != ints["c.lifecycle_plan_version"] ||
		ints["c.catalog_version"] < 1 || ints["c.billing_price_version"] < 1 || ints["c.policy_version"] != 3 ||
		!canonicalTimeField(n["effective_at"]) || !sameCanonical(n["effective_at"], c["effective_at"]) ||
		!sameCanonical(n["target_lifecycle"], c["target_lifecycle"]) || !sameCanonical(n["existing_session_cutoff_at"], c["existing_session_cutoff_at"]) ||
		!sameCanonical(n["replacement_model_id"], c["replacement_model_id"]) || !nullableUUID(n["replacement_model_id"]) ||
		n["force_cancel_inflight"].kind != jsonBool || c["force_cancel_inflight"].kind != jsonBool || !sameCanonical(n["force_cancel_inflight"], c["force_cancel_inflight"]) ||
		!hexField(c["route_set_fingerprint"]) || !hexField(c["provisioning_set_fingerprint"]) || !hexField(c["inflight_request_set_fingerprint"]) {
		return ErrInvalidDocument
	}
	target, _ := stringValue(n["target_lifecycle"])
	current, _ := stringValue(c["current_lifecycle"])
	if !(current == "PUBLISHED" && (target == "DEPRECATED" || target == "REMOVED") || current == "DEPRECATED" && target == "REMOVED") {
		return ErrInvalidDocument
	}
	if target == "REMOVED" {
		if !validUUIDField(n["replacement_model_id"]) || equalString(n["replacement_model_id"], targetID) || !sameCanonical(n["existing_session_cutoff_at"], n["effective_at"]) {
			return ErrInvalidDocument
		}
	} else {
		if n["replacement_model_id"].kind != jsonNull || n["force_cancel_inflight"].boolean {
			return ErrInvalidDocument
		}
		if n["existing_session_cutoff_at"].kind != jsonNull {
			if !canonicalTimeField(n["existing_session_cutoff_at"]) {
				return ErrInvalidDocument
			}
			cutoff, _ := time.Parse(time.RFC3339Nano, n["existing_session_cutoff_at"].text)
			effective, _ := time.Parse(time.RFC3339Nano, n["effective_at"].text)
			if cutoff.Before(effective) {
				return ErrInvalidDocument
			}
		}
	}
	if ints["f.billing_price_revision"] != ints["c.billing_price_version"] || ints["f.models_checked"] < 1 || ints["f.models_checked"] != ints["f.models_covered"] ||
		!boolEquals(f["fully_covered"], true) || !hexField(f["price_set_fingerprint"]) || !canonicalTimeField(f["billing_as_of"]) {
		return ErrInvalidDocument
	}
	return nil
}
