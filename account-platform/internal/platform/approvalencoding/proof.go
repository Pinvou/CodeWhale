package approvalencoding

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

type commandSpec struct {
	policyVersion        int64
	snapshotVersion      int
	targetType           string
	executor             string
	normalizedKeys       []string
	criticalKeys         []string
	financialKeys        []string
	normalizedInts       []string
	criticalInts         []string
	financialInts        []string
	nullableCriticalInts []string
}

var commandSpecs = map[string]commandSpec{
	"ENTITLEMENT_COMPONENT_ADJUSTMENT": {
		policyVersion: 2, snapshotVersion: 2, targetType: "ENTITLEMENT_PACKAGE", executor: "billing-command-executor",
		normalizedKeys: []string{"package_id", "payment_order_id", "reason_code"},
		criticalKeys:   []string{"package_id", "wallet_id", "wallet_version", "payment_order_id", "payment_transaction_id", "source_component_id", "source_lot_id", "target_component_id", "target_lot_id", "source_type", "target_type", "amount_microcredits", "source_evidence_digest", "facts_digest", "policy_version"},
		financialKeys:  []string{"package_total_microcredits", "source_original_microcredits", "target_original_microcredits", "effective_total_microcredits"},
		criticalInts:   []string{"wallet_version", "amount_microcredits", "policy_version"},
		financialInts:  []string{"package_total_microcredits", "source_original_microcredits", "target_original_microcredits", "effective_total_microcredits"},
	},
	"WALLET_ADJUSTMENT": {
		policyVersion: 1, snapshotVersion: 1, targetType: "MANUAL_ADJUSTMENT", executor: "billing-command-executor",
		normalizedKeys: []string{"adjustment_id", "wallet_id", "amount_microcredits", "requested_by"},
		criticalKeys:   []string{"adjustment_version"}, financialKeys: []string{"wallet_id", "delta_microcredits"},
		normalizedInts: []string{"amount_microcredits"}, criticalInts: []string{"adjustment_version"}, financialInts: []string{"delta_microcredits"},
	},
	"PAYMENT_PROVIDER_RESUME": {
		policyVersion: 2, snapshotVersion: 1, targetType: "PAYMENT_PROVIDER_CONTROL", executor: "billing-command-executor",
		normalizedKeys: []string{"provider_id", "provider_version", "provider_control_version", "probe_order_id", "policy_version", "observation_high_watermark", "evaluation_high_watermark", "active_mapping_set_fingerprint"},
		criticalKeys:   []string{"provider_id", "provider_version", "provider_control_version", "probe_order_id", "policy_version", "observation_high_watermark", "evaluation_high_watermark", "active_mapping_set_fingerprint"},
		financialKeys:  []string{"new_orders_enabled", "provider_control_version", "product_control_version"},
		normalizedInts: []string{"provider_control_version"}, criticalInts: []string{"provider_control_version"}, financialInts: []string{"provider_control_version"},
	},
	"PAYMENT_PRODUCT_RESUME": {
		policyVersion: 1, snapshotVersion: 1, targetType: "PAYMENT_PRODUCT_CONTROL", executor: "billing-command-executor",
		normalizedKeys: []string{"provider_id", "provider_version", "product_id", "provider_control_version", "product_control_version", "catalog_item_id", "catalog_version", "admission_version", "mapping_id", "mapping_version", "probe_order_id", "policy_version", "observation_high_watermark", "evaluation_high_watermark"},
		criticalKeys:   []string{"provider_id", "provider_version", "product_id", "provider_control_version", "product_control_version", "catalog_item_id", "catalog_version", "admission_version", "mapping_id", "mapping_version", "probe_order_id", "policy_version", "observation_high_watermark", "evaluation_high_watermark"},
		financialKeys:  []string{"new_orders_enabled", "provider_control_version", "product_control_version"},
		normalizedInts: []string{"provider_control_version", "product_control_version", "catalog_version", "admission_version", "mapping_version"},
		criticalInts:   []string{"provider_control_version", "product_control_version", "catalog_version", "admission_version", "mapping_version"},
		financialInts:  []string{"provider_control_version", "product_control_version"},
	},
	"TOKEN_MODEL_LIFECYCLE_CHANGE": {
		policyVersion: 2, snapshotVersion: 2, targetType: "MODEL", executor: "token-command-executor",
		normalizedKeys: []string{"model_id", "lifecycle_plan_id", "lifecycle_plan_version", "target_lifecycle", "effective_at", "existing_session_cutoff_at", "replacement_model_id"},
		criticalKeys:   []string{"model_id", "catalog_version", "current_lifecycle", "target_lifecycle", "lifecycle_plan_version", "effective_at", "existing_session_cutoff_at", "replacement_model_id", "billing_price_version", "route_set_fingerprint", "provisioning_set_fingerprint", "inflight_request_set_fingerprint", "policy_version"},
		financialKeys:  []string{"billing_price_revision", "price_set_fingerprint", "models_checked", "models_covered", "fully_covered", "billing_as_of"},
		normalizedInts: []string{"lifecycle_plan_version"}, criticalInts: []string{"catalog_version", "lifecycle_plan_version", "billing_price_version", "policy_version"}, financialInts: []string{"billing_price_revision", "models_checked", "models_covered"},
	},
	"TOKEN_MODEL_CATALOG_ROLLBACK": {
		policyVersion: 2, snapshotVersion: 2, targetType: "MODEL_CATALOG", executor: "token-command-executor",
		normalizedKeys: []string{"source_catalog_version"},
		criticalKeys:   []string{"current_catalog_version", "source_catalog_version", "catalog_diff_fingerprint", "billing_price_version", "route_set_fingerprint", "provisioning_set_fingerprint", "inflight_request_set_fingerprint", "policy_version"},
		financialKeys:  []string{"billing_price_revision", "price_set_fingerprint", "models_checked", "models_covered", "fully_covered", "billing_as_of"},
		normalizedInts: []string{"source_catalog_version"}, criticalInts: []string{"current_catalog_version", "source_catalog_version", "billing_price_version", "policy_version"}, financialInts: []string{"billing_price_revision", "models_checked", "models_covered"},
	},
	"TOKEN_CREDENTIAL_REVOKE": {
		policyVersion: 1, snapshotVersion: 1, targetType: "PROVIDER_CREDENTIAL", executor: "token-command-executor",
		normalizedKeys: []string{"token_account_id", "expected_account_version", "credential_id", "credential_version"},
		criticalKeys:   []string{"token_account_id", "token_account_version", "credential_id", "credential_version", "credential_status", "credential_fingerprint", "active_route_set_fingerprint", "provisioning_lease_set_fingerprint", "inflight_request_set_fingerprint", "replacement_credential_version", "policy_version"},
		financialKeys:  []string{},
		normalizedInts: []string{"expected_account_version", "credential_version"}, criticalInts: []string{"token_account_version", "credential_version", "policy_version"}, nullableCriticalInts: []string{"replacement_credential_version"},
	},
}

func ConvertLegacyProofInput(input LegacyProofInput) (ProofArtifacts, error) {
	if len(input.NormalizedCommand)+len(input.CriticalResourceVersions)+len(input.FinancialSnapshot) > MaximumDocumentBytes {
		return ProofArtifacts{}, ErrDocumentTooLarge
	}
	normalized, err := parseDocument(input.NormalizedCommand)
	if err != nil {
		return ProofArtifacts{}, err
	}
	critical, err := parseDocument(input.CriticalResourceVersions)
	if err != nil {
		return ProofArtifacts{}, err
	}
	financial, err := parseDocument(input.FinancialSnapshot)
	if err != nil {
		return ProofArtifacts{}, err
	}
	spec, ok := commandSpecs[input.CommandType]
	if !ok {
		return ProofArtifacts{}, ErrUnsupportedCommand
	}

	if err := convertLegacyIntegers(spec, normalized, critical, financial); err != nil {
		return ProofArtifacts{}, err
	}
	return buildProofArtifacts(input.CommandType, input.PolicyVersion, input.SnapshotSchemaVersion, input.TargetResourceType, input.TargetResourceID, normalized, critical, financial)
}

// Legacy plan persistence may contain an RFC3339 offset while the owner
// precheck stores its equivalent UTC instant. Proof schema 1 freezes the UTC
// representation; only these registered time fields may be normalized.
// LegacyExecutionInput converts a fully validated authorization event back to
// the existing domain-command JSON representation. The proof wire represents
// every int64 as a canonical decimal string; domain command handlers still own
// the original JSON-number contract. This conversion is closed by commandSpec
// and never guesses fields from their runtime shape.

func LegacyExecutionInput(decoded DecodedAuthorizationEvent) (LegacyProofInput, error) {
	if decoded.SerializationVersion != ApprovalSerializationVersion {
		return LegacyProofInput{}, ErrUnsupportedVersion
	}
	event := decoded.Payload
	spec, ok := commandSpecs[event.CommandType]
	if !ok || event.PolicyVersion != spec.policyVersion {
		return LegacyProofInput{}, ErrUnsupportedCommand
	}
	normalized, err := parseDocument(event.NormalizedCommand)
	if err != nil {
		return LegacyProofInput{}, err
	}
	critical, err := parseDocument(event.CriticalResourceVersions)
	if err != nil {
		return LegacyProofInput{}, err
	}
	financial, err := parseDocument(event.FinancialSnapshot)
	if err != nil {
		return LegacyProofInput{}, err
	}
	artifacts, err := buildProofArtifacts(event.CommandType, event.PolicyVersion, spec.snapshotVersion, event.TargetResourceType, event.TargetResourceID, normalized, critical, financial)
	if err != nil || artifacts.CommandDigest != event.CommandDigest || artifacts.SnapshotDigest != event.SnapshotDigest {
		return LegacyProofInput{}, ErrInvalidDocument
	}
	if err := convertProofIntegers(spec, normalized, critical, financial); err != nil {
		return LegacyProofInput{}, err
	}
	return LegacyProofInput{CommandType: event.CommandType, PolicyVersion: event.PolicyVersion, SnapshotSchemaVersion: spec.snapshotVersion,
		TargetResourceType: event.TargetResourceType, TargetResourceID: event.TargetResourceID,
		NormalizedCommand: rawCanonical(normalized), CriticalResourceVersions: rawCanonical(critical), FinancialSnapshot: rawCanonical(financial)}, nil
}

// LegacyProofInputFromArtifacts converts a validated proof request to the
// existing owner-domain precheck representation. It is only an adapter for
// reading authoritative facts; it must never substitute the supplied snapshot
// for an owner-domain query or bypass the later execution revision check.
func LegacyProofInputFromArtifacts(artifacts ProofArtifacts) (LegacyProofInput, error) {
	verified, err := DecodeProofInput(artifacts.CommandType, artifacts.PolicyVersion, artifacts.ProofInputJSON)
	if err != nil || !sameArtifactDigests(verified, artifacts) || verified.SnapshotSchemaVersion != artifacts.SnapshotSchemaVersion {
		return LegacyProofInput{}, ErrInvalidDocument
	}
	spec := commandSpecs[artifacts.CommandType]
	root, err := parseDocument(verified.ProofInputJSON)
	if err != nil {
		return LegacyProofInput{}, err
	}
	object, err := requireObject(root, "snapshot_schema_version", "target", "normalized_command", "critical_resource_versions", "financial_snapshot")
	if err != nil {
		return LegacyProofInput{}, err
	}
	target, err := requireObject(object["target"], "resource_type", "resource_id")
	if err != nil {
		return LegacyProofInput{}, err
	}
	targetType, typeErr := stringValue(target["resource_type"])
	targetID, idErr := stringValue(target["resource_id"])
	if typeErr != nil || idErr != nil {
		return LegacyProofInput{}, ErrInvalidDocument
	}
	normalized, critical, financial := object["normalized_command"], object["critical_resource_versions"], object["financial_snapshot"]
	if err := convertProofIntegers(spec, normalized, critical, financial); err != nil {
		return LegacyProofInput{}, err
	}

	return LegacyProofInput{
		CommandType: artifacts.CommandType, PolicyVersion: artifacts.PolicyVersion, SnapshotSchemaVersion: artifacts.SnapshotSchemaVersion,
		TargetResourceType: targetType, TargetResourceID: targetID,
		NormalizedCommand: rawCanonical(normalized), CriticalResourceVersions: rawCanonical(critical), FinancialSnapshot: rawCanonical(financial),
	}, nil
}

func sameArtifactDigests(left, right ProofArtifacts) bool {
	return left.CommandType == right.CommandType && left.PolicyVersion == right.PolicyVersion &&
		left.CommandDigest == right.CommandDigest && left.SnapshotDigest == right.SnapshotDigest
}

func convertProofIntegers(spec commandSpec, normalized, critical, financial *jsonValue) error {
	normalizedObject, err := objectValue(normalized)
	if err != nil {
		return err
	}
	criticalObject, err := objectValue(critical)
	if err != nil {
		return err
	}
	financialObject, err := objectValue(financial)
	if err != nil {
		return err
	}
	for _, fields := range []struct {
		object map[string]*jsonValue
		names  []string
	}{{normalizedObject, spec.normalizedInts}, {criticalObject, spec.criticalInts}, {financialObject, spec.financialInts}} {
		for _, field := range fields.names {
			if err := proofIntegerToLegacy(fields.object, field); err != nil {
				return err
			}
		}
	}
	for _, field := range spec.nullableCriticalInts {
		value, present := criticalObject[field]
		if !present {
			return ErrInvalidDocument
		}
		if value.kind != jsonNull {
			if err := proofIntegerToLegacy(criticalObject, field); err != nil {
				return err
			}
		}
	}
	return nil
}

func proofIntegerToLegacy(object map[string]*jsonValue, field string) error {
	value, present := object[field]
	if !present {
		return ErrInvalidDocument
	}
	integer, err := decimalString(value)
	if err != nil {
		return err
	}
	*value = *jsonInteger(integer)
	return nil
}

func DecodeProofInput(commandType string, policyVersion int64, raw []byte) (ProofArtifacts, error) {
	root, err := parseDocument(raw)
	if err != nil {
		return ProofArtifacts{}, err
	}
	object, err := requireObject(root, "snapshot_schema_version", "target", "normalized_command", "critical_resource_versions", "financial_snapshot")
	if err != nil {
		return ProofArtifacts{}, err
	}
	snapshotVersion, err := integerNumber(object["snapshot_schema_version"])
	if err != nil || snapshotVersion < 1 || snapshotVersion > int64(^uint(0)>>1) {
		return ProofArtifacts{}, ErrInvalidDocument
	}
	target, err := requireObject(object["target"], "resource_type", "resource_id")
	if err != nil {
		return ProofArtifacts{}, err
	}
	targetType, err := stringValue(target["resource_type"])
	if err != nil {
		return ProofArtifacts{}, err
	}
	targetID, err := stringValue(target["resource_id"])
	if err != nil {
		return ProofArtifacts{}, err
	}
	return buildProofArtifacts(commandType, policyVersion, int(snapshotVersion), targetType, targetID, object["normalized_command"], object["critical_resource_versions"], object["financial_snapshot"])
}

func buildProofArtifacts(commandType string, policyVersion int64, snapshotVersion int, targetType, targetID string, normalized, critical, financial *jsonValue) (ProofArtifacts, error) {
	spec, ok := commandSpecs[commandType]
	if !ok {
		return ProofArtifacts{}, ErrUnsupportedCommand
	}
	if spec.policyVersion != policyVersion || spec.snapshotVersion != snapshotVersion {
		return ProofArtifacts{}, ErrUnsupportedVersion
	}
	if err := validateProofValues(commandType, spec, targetType, targetID, normalized, critical, financial); err != nil {
		return ProofArtifacts{}, err
	}
	target := newObject(map[string]*jsonValue{"resource_type": jsonString(targetType), "resource_id": jsonString(targetID)})
	proof := newObject(map[string]*jsonValue{
		"snapshot_schema_version": jsonInteger(int64(snapshotVersion)), "target": target,
		"normalized_command": normalized, "critical_resource_versions": critical, "financial_snapshot": financial,
	})
	command := newObject(map[string]*jsonValue{
		"command_type": jsonString(commandType), "policy_version": jsonInteger(policyVersion), "target": cloneValue(target), "normalized_command": cloneValue(normalized),
	})
	snapshot := newObject(map[string]*jsonValue{
		"snapshot_schema_version": jsonInteger(int64(snapshotVersion)), "critical_resource_versions": cloneValue(critical), "financial_snapshot": cloneValue(financial),
	})
	proofJSON, err := canonicalJSON(proof)
	if err != nil {
		return ProofArtifacts{}, err
	}
	if len(proofJSON) > MaximumDocumentBytes {
		return ProofArtifacts{}, ErrDocumentTooLarge
	}
	commandJCS, err := canonicalJSON(command)
	if err != nil {
		return ProofArtifacts{}, err
	}
	snapshotJCS, err := canonicalJSON(snapshot)
	if err != nil {
		return ProofArtifacts{}, err
	}
	return ProofArtifacts{
		CommandType: commandType, PolicyVersion: policyVersion, SnapshotSchemaVersion: snapshotVersion,
		ProofInputJSON: proofJSON, CommandJCS: commandJCS, SnapshotJCS: snapshotJCS,
		CommandDigest: digestHex(commandJCS), SnapshotDigest: digestHex(snapshotJCS),
	}, nil
}

func convertLegacyIntegers(spec commandSpec, normalized, critical, financial *jsonValue) error {
	normalizedObject, err := objectValue(normalized)
	if err != nil {
		return err
	}
	criticalObject, err := objectValue(critical)
	if err != nil {
		return err
	}
	financialObject, err := objectValue(financial)
	if err != nil {
		return err
	}
	for _, field := range spec.normalizedInts {
		if _, err := convertLegacyInteger(normalizedObject, field); err != nil {
			return err
		}
	}
	for _, field := range spec.criticalInts {
		if _, err := convertLegacyInteger(criticalObject, field); err != nil {
			return err
		}
	}
	for _, field := range spec.financialInts {
		if _, err := convertLegacyInteger(financialObject, field); err != nil {
			return err
		}
	}
	for _, field := range spec.nullableCriticalInts {
		value, ok := criticalObject[field]
		if !ok {
			return ErrInvalidDocument
		}
		if value.kind != jsonNull {
			if _, err := convertLegacyInteger(criticalObject, field); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateProofValues(commandType string, spec commandSpec, targetType, targetID string, normalized, critical, financial *jsonValue) error {
	if targetType != spec.targetType || targetID == "" {
		return ErrInvalidDocument
	}
	normalizedObject, err := requireObject(normalized, spec.normalizedKeys...)
	if err != nil {
		return err
	}
	criticalObject, err := requireObject(critical, spec.criticalKeys...)
	if err != nil {
		return err
	}
	financialObject, err := requireObject(financial, spec.financialKeys...)
	if err != nil {
		return err
	}
	integers := make(map[string]int64)
	for _, field := range spec.normalizedInts {
		value, err := decimalString(normalizedObject[field])
		if err != nil {
			return err
		}
		integers["n."+field] = value
	}
	for _, field := range spec.criticalInts {
		value, err := decimalString(criticalObject[field])
		if err != nil {
			return err
		}
		integers["c."+field] = value
	}
	for _, field := range spec.financialInts {
		value, err := decimalString(financialObject[field])
		if err != nil {
			return err
		}
		integers["f."+field] = value
	}
	for _, field := range spec.nullableCriticalInts {
		value := criticalObject[field]
		if value.kind != jsonNull {
			parsed, err := decimalString(value)
			if err != nil || parsed < 1 {
				return ErrInvalidDocument
			}
		}
	}
	switch commandType {
	case "ENTITLEMENT_COMPONENT_ADJUSTMENT":
		if !validUUID(targetID) || !equalString(normalizedObject["package_id"], targetID) || !equalString(criticalObject["package_id"], targetID) ||
			!validUUIDField(normalizedObject["payment_order_id"]) || !sameCanonical(normalizedObject["payment_order_id"], criticalObject["payment_order_id"]) || !boundedStringField(normalizedObject["reason_code"], 80) ||
			!validUUIDField(criticalObject["wallet_id"]) || !validUUIDField(criticalObject["payment_transaction_id"]) || !validUUIDField(criticalObject["source_component_id"]) || !validUUIDField(criticalObject["source_lot_id"]) ||
			!nullableUUID(criticalObject["target_component_id"]) || !nullableUUID(criticalObject["target_lot_id"]) || (criticalObject["target_component_id"].kind == jsonNull) != (criticalObject["target_lot_id"].kind == jsonNull) ||
			!entitlementComponentKind(criticalObject["source_type"]) || !entitlementComponentKind(criticalObject["target_type"]) || sameCanonical(criticalObject["source_type"], criticalObject["target_type"]) ||
			!hexLengthField(criticalObject["source_evidence_digest"], 64) || !hexLengthField(criticalObject["facts_digest"], 64) || integers["c.wallet_version"] < 1 || integers["c.amount_microcredits"] < 1 || integers["c.policy_version"] != 2 ||
			integers["f.package_total_microcredits"] < 1 || integers["f.effective_total_microcredits"] != integers["f.package_total_microcredits"] || integers["f.source_original_microcredits"] < integers["c.amount_microcredits"] || integers["f.target_original_microcredits"] < 0 {
			return ErrInvalidDocument
		}
	case "WALLET_ADJUSTMENT":
		if !validUUID(targetID) || !equalString(normalizedObject["adjustment_id"], targetID) || !validUUIDField(normalizedObject["wallet_id"]) || !validUUIDField(financialObject["wallet_id"]) || !sameCanonical(normalizedObject["wallet_id"], financialObject["wallet_id"]) || integers["n.amount_microcredits"] == 0 || integers["n.amount_microcredits"] != integers["f.delta_microcredits"] || integers["c.adjustment_version"] < 1 {
			return ErrInvalidDocument
		}
		if !validUUIDField(normalizedObject["requested_by"]) {
			return ErrInvalidDocument
		}
	case "PAYMENT_PROVIDER_RESUME":
		if !boundedString(targetID, 160) || !equalString(normalizedObject["provider_id"], targetID) || !sameCanonical(normalized, critical) || integers["n.provider_control_version"] < 1 || integers["n.provider_control_version"] != integers["f.provider_control_version"] || !providerCommon(normalizedObject) || !boolEquals(financialObject["new_orders_enabled"], false) || financialObject["product_control_version"].kind != jsonNull {
			return ErrInvalidDocument
		}
	case "PAYMENT_PRODUCT_RESUME":
		if !boundedString(targetID, 160) || !equalString(normalizedObject["product_id"], targetID) || !sameCanonical(normalized, critical) || integers["n.provider_control_version"] < 1 || integers["n.product_control_version"] < 1 || integers["n.catalog_version"] < 1 || integers["n.admission_version"] < 1 || integers["n.mapping_version"] < 1 || integers["n.provider_control_version"] != integers["f.provider_control_version"] || integers["n.product_control_version"] != integers["f.product_control_version"] || !productCommon(normalizedObject) || !boolEquals(financialObject["new_orders_enabled"], false) {
			return ErrInvalidDocument
		}

	}
	return nil
}

func entitlementComponentKind(value *jsonValue) bool {
	return equalString(value, "BASE") || equalString(value, "BONUS")
}

func providerCommon(object map[string]*jsonValue) bool {
	return boundedStringField(object["provider_id"], 160) && boundedStringField(object["provider_version"], 160) && boundedStringField(object["policy_version"], 160) && validUUIDField(object["probe_order_id"]) && nullableUUID(object["observation_high_watermark"]) && nullableUUID(object["evaluation_high_watermark"]) && hexLengthField(object["active_mapping_set_fingerprint"], 32)
}

func productCommon(object map[string]*jsonValue) bool {
	return boundedStringField(object["provider_id"], 160) && boundedStringField(object["provider_version"], 160) && boundedStringField(object["product_id"], 160) && boundedStringField(object["policy_version"], 160) && validUUIDField(object["catalog_item_id"]) && validUUIDField(object["mapping_id"]) && validUUIDField(object["probe_order_id"]) && nullableUUID(object["observation_high_watermark"]) && nullableUUID(object["evaluation_high_watermark"])
}

// DeriveProofDigest validates the closed transport envelope and derives its
// local digest. Callers must first apply the command-specific domain proof
// reason whitelist; the shared DENY union here is not an authorization check.
func DeriveProofDigest(response ProofResponse) ([32]byte, error) {
	spec, ok := commandSpecs[response.CommandType]
	if !ok || response.PolicyVersion != spec.policyVersion || response.ApprovalRequestID == uuid.Nil || response.ProofSchemaVersion != ProofSchemaVersion || !hexString(response.CommandDigest) || !hexString(response.SnapshotDigest) || !boundedString(response.DomainRevision, 128) || !canonicalUTC(response.IssuedAt) || !canonicalUTC(response.ExpiresAt) || !response.ExpiresAt.Equal(response.IssuedAt.Add(time.Minute)) {
		return [32]byte{}, ErrInvalidDocument
	}
	if response.Verdict == "ALLOW" {
		if response.ReasonCode != "PROOF_VALID" {
			return [32]byte{}, ErrInvalidDocument
		}
	} else if response.Verdict == "DENY" {
		if !oneOf(response.ReasonCode, "RESOURCE_NOT_FOUND", "RESOURCE_STATE_CHANGED", "RESOURCE_VERSION_CHANGED", "SNAPSHOT_MISMATCH", "POLICY_VERSION_CHANGED", "ACTIVATION_CHANGED", "DOMAIN_INVARIANT_FAILED", "EXECUTION_WINDOW_EXPIRED", "EVIDENCE_MISSING", "EVIDENCE_EXPIRED", "PROVIDER_STATE_CHANGED") {
			return [32]byte{}, ErrInvalidDocument
		}
	} else {
		return [32]byte{}, ErrInvalidDocument
	}
	value := newObject(map[string]*jsonValue{
		"approval_request_id": jsonString(response.ApprovalRequestID.String()), "command_type": jsonString(response.CommandType), "command_digest": jsonString(response.CommandDigest), "snapshot_digest": jsonString(response.SnapshotDigest),
		"policy_version": jsonInteger(response.PolicyVersion), "proof_schema_version": jsonString(response.ProofSchemaVersion), "domain_revision": jsonString(response.DomainRevision), "verdict": jsonString(response.Verdict), "reason_code": jsonString(response.ReasonCode),
		"issued_at": jsonString(response.IssuedAt.UTC().Format(time.RFC3339Nano)), "expires_at": jsonString(response.ExpiresAt.UTC().Format(time.RFC3339Nano)),
	})
	canonical, err := canonicalJSON(value)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(canonical), nil
}

func digestHex(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func sameCanonical(left, right *jsonValue) bool {
	leftBytes, leftErr := canonicalJSON(left)
	rightBytes, rightErr := canonicalJSON(right)
	return leftErr == nil && rightErr == nil && string(leftBytes) == string(rightBytes)
}

func equalString(value *jsonValue, expected string) bool {
	actual, err := stringValue(value)
	return err == nil && actual == expected
}

func validUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed != uuid.Nil && parsed.String() == value
}

func validUUIDField(value *jsonValue) bool {
	text, err := stringValue(value)
	return err == nil && validUUID(text)
}

func nullableUUID(value *jsonValue) bool {
	return value != nil && (value.kind == jsonNull || validUUIDField(value))
}

func boundedString(value string, maximum int) bool {
	return utf8.ValidString(value) && value != "" && utf8.RuneCountInString(value) <= maximum
}

func boundedStringField(value *jsonValue, maximum int) bool {
	text, err := stringValue(value)
	return err == nil && boundedString(text, maximum)
}

func hexString(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func hexField(value *jsonValue) bool {
	return hexLengthField(value, 64)
}

func hexLengthField(value *jsonValue, length int) bool {
	text, err := stringValue(value)
	if err != nil || len(text) != length || strings.ToLower(text) != text {
		return false
	}
	_, err = hex.DecodeString(text)
	return err == nil
}

func boolEquals(value *jsonValue, expected bool) bool {
	return value != nil && value.kind == jsonBool && value.boolean == expected
}

func oneOfString(value *jsonValue, allowed ...string) bool {
	text, err := stringValue(value)
	return err == nil && oneOf(text, allowed...)
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func canonicalTimeField(value *jsonValue) bool {
	text, err := stringValue(value)
	if err != nil {
		return false
	}
	return canonicalTimeString(text)
}

func nullableTime(value *jsonValue) bool {
	return value != nil && (value.kind == jsonNull || canonicalTimeField(value))
}

func canonicalTimeString(value string) bool {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return err == nil && !parsed.IsZero() && strings.HasSuffix(value, "Z") && parsed.UTC().Format(time.RFC3339Nano) == value
}

func requiredTime(value *jsonValue) (time.Time, bool) {
	if value == nil || value.kind != jsonStringKind {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, value.text)
	return parsed, err == nil && !parsed.IsZero() && strings.HasSuffix(value.text, "Z") && parsed.UTC().Format(time.RFC3339Nano) == value.text
}

func optionalTime(value *jsonValue) (time.Time, bool, bool) {
	if value == nil {
		return time.Time{}, false, false
	}
	if value.kind == jsonNull {
		return time.Time{}, false, true
	}
	parsed, ok := requiredTime(value)
	return parsed, true, ok
}

func canonicalUTC(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC
}

func rawCanonical(value *jsonValue) json.RawMessage {
	canonical, _ := canonicalJSON(value)
	return json.RawMessage(canonical)
}
