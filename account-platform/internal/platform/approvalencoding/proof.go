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
	"ENTITLEMENT_COMPONENT_ADJUSTMENT": {
		policyVersion: 2, snapshotVersion: 2, targetType: "ENTITLEMENT_PACKAGE", executor: "billing-command-executor",
		normalizedKeys: []string{"package_id", "payment_order_id", "reason_code"},
		criticalKeys: []string{"package_id", "wallet_id", "wallet_version", "payment_order_id", "payment_transaction_id",
			"source_component_id", "source_lot_id", "target_component_id", "target_lot_id", "source_type", "target_type",
			"amount_microcredits", "source_evidence_digest", "facts_digest", "policy_version"},
		financialKeys: []string{"package_total_microcredits", "source_original_microcredits", "target_original_microcredits", "effective_total_microcredits"},
		criticalInts:  []string{"wallet_version", "amount_microcredits", "policy_version"},
		financialInts: []string{"package_total_microcredits", "source_original_microcredits", "target_original_microcredits", "effective_total_microcredits"},
	},
	// LEDGER_REPAIR reuses the command's original v1 policy/schema. The target
	// is the immutable posting, and every amount or account type is recomputed
	// by the owner proof, so the closed wire carries identifiers plus the CAS
	// posting-set digest and the frozen amount.
	"LEDGER_REPAIR": {
		policyVersion: 1, snapshotVersion: 1, targetType: "LEDGER_POSTING", executor: "billing-command-executor",
		normalizedKeys: []string{"target_journal_entry_id", "target_posting_id", "wrong_ledger_account_id", "correct_ledger_account_id", "amount_microcredits", "reason_code"},
		criticalKeys:   []string{"target_journal_entry_id", "target_posting_id", "posting_set_digest", "entry_type", "policy_version"},
		financialKeys:  []string{"amount_microcredits", "wrong_ledger_account_id", "correct_ledger_account_id", "wrong_account_type", "correct_account_type", "wallet_id"},
		normalizedInts: []string{"amount_microcredits"}, criticalInts: []string{"policy_version"}, financialInts: []string{"amount_microcredits"},
	},
	// RECEIVABLE_WRITE_OFF reuses the command's original v1 policy/schema. The
	// target is the receivable, and every amount, the wallet projection and the
	// facts digest are recomputed by the owner proof.
	"RECEIVABLE_WRITE_OFF": {
		policyVersion: 1, snapshotVersion: 1, targetType: "RECEIVABLE", executor: "billing-command-executor",
		normalizedKeys: []string{"receivable_id", "write_off_microcredits", "expected_receivable_version", "reason_code"},
		criticalKeys:   []string{"receivable_version", "facts_digest", "policy_version"},
		financialKeys: []string{"write_off_microcredits", "outstanding_microcredits", "settled_microcredits",
			"original_microcredits", "source_type", "source_reference", "wallet_receivable_microcredits"},
		normalizedInts: []string{"write_off_microcredits", "expected_receivable_version"},
		criticalInts:   []string{"receivable_version", "policy_version"},
		financialInts: []string{"write_off_microcredits", "outstanding_microcredits", "settled_microcredits",
			"original_microcredits", "wallet_receivable_microcredits"},
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
	if input.CommandType == "TOKEN_MODEL_LIFECYCLE_CHANGE" {
		if err := deriveLegacyLifecyclePlan(normalized, critical); err != nil {
			return ProofArtifacts{}, err
		}
		if err := normalizeLegacyProofTimes(normalized, "effective_at", "existing_session_cutoff_at"); err != nil {
			return ProofArtifacts{}, err
		}
		if err := normalizeLegacyProofTimes(critical, "effective_at", "existing_session_cutoff_at"); err != nil {
			return ProofArtifacts{}, err
		}
	}
	if input.CommandType == "TOKEN_MODEL_LIFECYCLE_CHANGE" || input.CommandType == "TOKEN_MODEL_CATALOG_ROLLBACK" {
		if err := normalizeLegacyProofTimes(financial, "billing_as_of"); err != nil {
			return ProofArtifacts{}, err
		}
	}
	if err := convertLegacyIntegers(spec, normalized, critical, financial); err != nil {
		return ProofArtifacts{}, err
	}
	return buildProofArtifacts(input.CommandType, input.PolicyVersion, input.SnapshotSchemaVersion, input.TargetResourceType, input.TargetResourceID, normalized, critical, financial)
}

// Legacy plan persistence may contain an RFC3339 offset while the owner
// precheck stores its equivalent UTC instant. Proof schema 1 freezes the UTC
// representation; only these registered time fields may be normalized.
func normalizeLegacyProofTimes(value *jsonValue, names ...string) error {
	fields, err := objectValue(value)
	if err != nil {
		return err
	}
	for _, name := range names {
		field, ok := fields[name]
		if !ok {
			return ErrInvalidDocument
		}
		if field.kind == jsonNull && name == "existing_session_cutoff_at" {
			continue
		}
		raw, err := stringValue(field)
		if err != nil {
			return err
		}
		instant, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil || instant.IsZero() {
			return ErrInvalidDocument
		}
		*field = *jsonString(instant.UTC().Format(time.RFC3339Nano))
	}
	return nil
}

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
	if artifacts.CommandType == "TOKEN_MODEL_LIFECYCLE_CHANGE" {
		// The existing instantaneous precheck represents no plan by omitted
		// fields; proof schema 1 represents the same state by explicit null/0.
		values, err := objectValue(normalized)
		if err != nil {
			return LegacyProofInput{}, err
		}
		if values["lifecycle_plan_id"].kind == jsonNull {
			delete(values, "lifecycle_plan_id")
			delete(values, "lifecycle_plan_version")
			normalized = newObject(values)
		}
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

func deriveLegacyLifecyclePlan(normalized, critical *jsonValue) error {
	normalizedObject, err := objectValue(normalized)
	if err != nil {
		return err
	}
	_, hasID := normalizedObject["lifecycle_plan_id"]
	_, hasVersion := normalizedObject["lifecycle_plan_version"]
	if hasID != hasVersion {
		return ErrInvalidDocument
	}
	if hasID {
		return nil
	}
	criticalObject, err := objectValue(critical)
	if err != nil {
		return err
	}
	criticalVersion, err := integerNumber(criticalObject["lifecycle_plan_version"])
	if err != nil || criticalVersion != 0 {
		return ErrInvalidDocument
	}
	normalized.object = append(normalized.object,
		jsonMember{name: "lifecycle_plan_id", value: jsonNullValue()},
		jsonMember{name: "lifecycle_plan_version", value: jsonInteger(0)},
	)
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
	case "TOKEN_MODEL_LIFECYCLE_CHANGE":
		if !validUUID(targetID) || !equalString(normalizedObject["model_id"], targetID) || !equalString(criticalObject["model_id"], targetID) || integers["n.lifecycle_plan_version"] < 0 || integers["c.catalog_version"] < 1 || integers["c.billing_price_version"] < 1 || integers["c.policy_version"] != spec.policyVersion || integers["f.billing_price_revision"] < 1 || !validateTokenFinancial(financialObject, integers) || !validateLifecycle(normalizedObject, criticalObject, integers) {
			return ErrInvalidDocument
		}
	case "TOKEN_MODEL_CATALOG_ROLLBACK":
		if integers["n.source_catalog_version"] < 1 || targetID != normalizedObject["source_catalog_version"].text || integers["c.current_catalog_version"] < 1 || integers["c.source_catalog_version"] != integers["n.source_catalog_version"] || integers["c.billing_price_version"] < 1 || integers["c.policy_version"] != spec.policyVersion || !hexField(criticalObject["catalog_diff_fingerprint"]) || !tokenSetFingerprints(criticalObject) || !validateTokenFinancial(financialObject, integers) {
			return ErrInvalidDocument
		}
	case "TOKEN_CREDENTIAL_REVOKE":
		if !validUUID(targetID) || !equalString(normalizedObject["credential_id"], targetID) || !equalString(criticalObject["credential_id"], targetID) || !validUUIDField(normalizedObject["token_account_id"]) || !sameCanonical(normalizedObject["token_account_id"], criticalObject["token_account_id"]) || integers["n.expected_account_version"] < 1 || integers["n.credential_version"] < 1 || integers["c.token_account_version"] != integers["n.expected_account_version"] || integers["c.credential_version"] != integers["n.credential_version"] || integers["c.policy_version"] != spec.policyVersion || !oneOfString(criticalObject["credential_status"], "PENDING", "ACTIVE", "RETIRED", "REVOKED") || !hexField(criticalObject["credential_fingerprint"]) || !hexField(criticalObject["active_route_set_fingerprint"]) || !hexField(criticalObject["provisioning_lease_set_fingerprint"]) || !hexField(criticalObject["inflight_request_set_fingerprint"]) {
			return ErrInvalidDocument
		}
	case "ENTITLEMENT_COMPONENT_ADJUSTMENT":
		sourceType, sourceErr := stringValue(criticalObject["source_type"])
		targetType, targetErr := stringValue(criticalObject["target_type"])
		reasonCode, reasonErr := stringValue(normalizedObject["reason_code"])
		if !validUUID(targetID) || !equalString(normalizedObject["package_id"], targetID) ||
			!sameCanonical(normalizedObject["package_id"], criticalObject["package_id"]) ||
			!validUUIDField(normalizedObject["payment_order_id"]) || !sameCanonical(normalizedObject["payment_order_id"], criticalObject["payment_order_id"]) ||
			!validUUIDField(criticalObject["wallet_id"]) || integers["c.wallet_version"] < 1 ||
			!validUUIDField(criticalObject["payment_transaction_id"]) ||
			!validUUIDField(criticalObject["source_component_id"]) || !validUUIDField(criticalObject["source_lot_id"]) ||
			!nullableUUID(criticalObject["target_component_id"]) || !nullableUUID(criticalObject["target_lot_id"]) ||
			sourceErr != nil || targetErr != nil || !((sourceType == "BASE" && targetType == "BONUS") || (sourceType == "BONUS" && targetType == "BASE")) ||
			!hexField(criticalObject["source_evidence_digest"]) || !hexField(criticalObject["facts_digest"]) ||
			integers["c.policy_version"] != spec.policyVersion || integers["c.amount_microcredits"] < 1 ||
			integers["c.amount_microcredits"] > integers["f.source_original_microcredits"] ||
			integers["f.package_total_microcredits"] < 1 || integers["f.source_original_microcredits"] < 1 || integers["f.target_original_microcredits"] < 0 ||
			integers["f.effective_total_microcredits"] != integers["f.source_original_microcredits"]+integers["f.target_original_microcredits"] ||
			integers["f.effective_total_microcredits"] != integers["f.package_total_microcredits"] ||
			reasonErr != nil || len(reasonCode) == 0 || len(reasonCode) > 80 {
			return ErrInvalidDocument
		}
	case "LEDGER_REPAIR":
		reasonCode, reasonErr := stringValue(normalizedObject["reason_code"])
		wrongAccountType, wrongAccountTypeErr := stringValue(financialObject["wrong_account_type"])
		correctAccountType, correctAccountTypeErr := stringValue(financialObject["correct_account_type"])
		if !validUUID(targetID) || !equalString(normalizedObject["target_posting_id"], targetID) ||
			!sameCanonical(normalizedObject["target_posting_id"], criticalObject["target_posting_id"]) ||
			!validUUIDField(normalizedObject["target_journal_entry_id"]) ||
			!sameCanonical(normalizedObject["target_journal_entry_id"], criticalObject["target_journal_entry_id"]) ||
			!validUUIDField(normalizedObject["wrong_ledger_account_id"]) ||
			!sameCanonical(normalizedObject["wrong_ledger_account_id"], financialObject["wrong_ledger_account_id"]) ||
			!validUUIDField(normalizedObject["correct_ledger_account_id"]) ||
			!sameCanonical(normalizedObject["correct_ledger_account_id"], financialObject["correct_ledger_account_id"]) ||
			sameCanonical(normalizedObject["wrong_ledger_account_id"], normalizedObject["correct_ledger_account_id"]) ||
			integers["n.amount_microcredits"] < 1 || integers["n.amount_microcredits"] != integers["f.amount_microcredits"] ||
			integers["c.policy_version"] != spec.policyVersion || !hexField(criticalObject["posting_set_digest"]) ||
			!oneOfString(criticalObject["entry_type"], "RECHARGE", "USAGE", "ADJUSTMENT", "REFUND", "EXPIRY", "CHARGEBACK", "REPAIR_CREDIT", "FORCED_REFUND") ||
			wrongAccountTypeErr != nil || correctAccountTypeErr != nil || wrongAccountType == "" || correctAccountType == "" ||
			financialObject["wallet_id"].kind != jsonNull ||
			reasonErr != nil || len(reasonCode) == 0 || len(reasonCode) > 80 {
			return ErrInvalidDocument
		}
	case "RECEIVABLE_WRITE_OFF":
		reasonCode, reasonErr := stringValue(normalizedObject["reason_code"])
		sourceType, sourceTypeErr := stringValue(financialObject["source_type"])
		sourceReference, sourceReferenceErr := stringValue(financialObject["source_reference"])
		if !validUUID(targetID) || !equalString(normalizedObject["receivable_id"], targetID) ||
			integers["n.expected_receivable_version"] < 1 || integers["n.expected_receivable_version"] != integers["c.receivable_version"] ||
			integers["c.policy_version"] != spec.policyVersion || !hexField(criticalObject["facts_digest"]) ||
			integers["n.write_off_microcredits"] < 1 || integers["n.write_off_microcredits"] != integers["f.write_off_microcredits"] ||
			integers["f.write_off_microcredits"] > integers["f.outstanding_microcredits"] ||
			integers["f.outstanding_microcredits"] < 1 || integers["f.settled_microcredits"] < 0 ||
			integers["f.original_microcredits"] < 1 ||
			integers["f.settled_microcredits"]+integers["f.outstanding_microcredits"] != integers["f.original_microcredits"] ||
			integers["f.wallet_receivable_microcredits"] < integers["f.write_off_microcredits"] ||
			sourceTypeErr != nil || sourceType != "USAGE_OVERAGE" ||
			sourceReferenceErr != nil || len(sourceReference) == 0 || len(sourceReference) > 200 ||
			reasonErr != nil || reasonCode != "APPROVED_RECEIVABLE_WRITE_OFF" {
			return ErrInvalidDocument
		}
	}
	return nil
}

func validateLifecycle(normalized, critical map[string]*jsonValue, values map[string]int64) bool {
	planID := normalized["lifecycle_plan_id"]
	if (planID.kind == jsonNull) != (values["n.lifecycle_plan_version"] == 0) {
		return false
	}
	if planID.kind != jsonNull && !validUUIDField(planID) {
		return false
	}
	if values["n.lifecycle_plan_version"] != values["c.lifecycle_plan_version"] || !sameCanonical(normalized["target_lifecycle"], critical["target_lifecycle"]) {
		return false
	}
	current, err := stringValue(critical["current_lifecycle"])
	if err != nil {
		return false
	}
	target, err := stringValue(normalized["target_lifecycle"])
	if err != nil || !((current == "PUBLISHED" && (target == "DEPRECATED" || target == "REMOVED")) || (current == "DEPRECATED" && target == "REMOVED")) {
		return false
	}
	effectiveAt, ok := requiredTime(normalized["effective_at"])
	if !ok || !sameCanonical(normalized["effective_at"], critical["effective_at"]) {
		return false
	}
	cutoffAt, hasCutoff, ok := optionalTime(normalized["existing_session_cutoff_at"])
	if !ok || !sameCanonical(normalized["existing_session_cutoff_at"], critical["existing_session_cutoff_at"]) {
		return false
	}
	replacement := normalized["replacement_model_id"]
	if !nullableUUID(replacement) || !sameCanonical(replacement, critical["replacement_model_id"]) {
		return false
	}
	modelID, _ := stringValue(normalized["model_id"])
	if replacement.kind != jsonNull {
		replacementID, _ := stringValue(replacement)
		if replacementID == modelID {
			return false
		}
	}
	if target == "REMOVED" {
		if replacement.kind == jsonNull || !hasCutoff || !cutoffAt.Equal(effectiveAt) {
			return false
		}
	} else if hasCutoff && cutoffAt.Before(effectiveAt) {
		return false
	}
	return tokenSetFingerprints(critical)
}

func validateTokenFinancial(object map[string]*jsonValue, values map[string]int64) bool {
	checked, covered := values["f.models_checked"], values["f.models_covered"]
	if checked < 0 || covered < 0 || covered > checked || values["f.billing_price_revision"] < 1 || !hexField(object["price_set_fingerprint"]) || !canonicalTimeField(object["billing_as_of"]) {
		return false
	}
	return boolEquals(object["fully_covered"], checked == covered)
}

func providerCommon(object map[string]*jsonValue) bool {
	return boundedStringField(object["provider_id"], 160) && boundedStringField(object["provider_version"], 160) && boundedStringField(object["policy_version"], 160) && validUUIDField(object["probe_order_id"]) && nullableUUID(object["observation_high_watermark"]) && nullableUUID(object["evaluation_high_watermark"]) && hexLengthField(object["active_mapping_set_fingerprint"], 32)
}

func productCommon(object map[string]*jsonValue) bool {
	return boundedStringField(object["provider_id"], 160) && boundedStringField(object["provider_version"], 160) && boundedStringField(object["product_id"], 160) && boundedStringField(object["policy_version"], 160) && validUUIDField(object["catalog_item_id"]) && validUUIDField(object["mapping_id"]) && validUUIDField(object["probe_order_id"]) && nullableUUID(object["observation_high_watermark"]) && nullableUUID(object["evaluation_high_watermark"])
}

func tokenSetFingerprints(object map[string]*jsonValue) bool {
	for _, field := range []string{"route_set_fingerprint", "provisioning_set_fingerprint", "inflight_request_set_fingerprint"} {
		if value, ok := object[field]; ok && !hexField(value) {
			return false
		}
	}
	return true
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
