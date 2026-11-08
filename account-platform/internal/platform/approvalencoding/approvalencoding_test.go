package approvalencoding

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
)

type goldenFile struct {
	SchemaVersion        int                         `json:"schema_version"`
	ProofVectors         []goldenProofVector         `json:"proof_vectors"`
	AuthorizationVectors []goldenAuthorizationVector `json:"authorization_vectors"`
	ProofResponseVectors []goldenProofResponseVector `json:"proof_response_vectors"`
}

type goldenProofVector struct {
	Name                  string       `json:"name"`
	CommandType           string       `json:"command_type"`
	PolicyVersion         int64        `json:"policy_version"`
	SnapshotSchemaVersion int          `json:"snapshot_schema_version"`
	Legacy                goldenLegacy `json:"legacy"`
	ProofJSON             string       `json:"proof_json"`
	CommandJCS            string       `json:"command_jcs"`
	SnapshotJCS           string       `json:"snapshot_jcs"`
	CommandDigest         string       `json:"command_digest"`
	SnapshotDigest        string       `json:"snapshot_digest"`
}

type goldenLegacy struct {
	TargetResourceType string `json:"target_resource_type"`
	TargetResourceID   string `json:"target_resource_id"`
	NormalizedJSON     string `json:"normalized_json"`
	CriticalJSON       string `json:"critical_json"`
	FinancialJSON      string `json:"financial_json"`
}

type goldenAuthorizationVector struct {
	Name                   string `json:"name"`
	SerializationVersion   string `json:"serialization_version"`
	EventJSON              string `json:"event_json"`
	EventDigest            string `json:"event_digest"`
	ApprovedRequestVersion string `json:"approved_request_version"`
	ApprovalEpoch          string `json:"approval_epoch"`
}

type goldenProofResponseVector struct {
	Name         string `json:"name"`
	ResponseJSON string `json:"response_json"`
	ProofDigest  string `json:"proof_digest"`
}

func frozenLegacyInputs() []struct {
	name  string
	input LegacyProofInput
} {
	return []struct {
		name  string
		input LegacyProofInput
	}{
		{"wallet-max-int64", LegacyProofInput{
			CommandType: "WALLET_ADJUSTMENT", PolicyVersion: 1, SnapshotSchemaVersion: 1, TargetResourceType: "MANUAL_ADJUSTMENT", TargetResourceID: "11111111-1111-4111-8111-111111111111",
			NormalizedCommand:        json.RawMessage(`{"adjustment_id":"11111111-1111-4111-8111-111111111111","wallet_id":"22222222-2222-4222-8222-222222222222","amount_microcredits":9223372036854775807,"requested_by":"33333333-3333-4333-8333-333333333333"}`),
			CriticalResourceVersions: json.RawMessage(`{"adjustment_version":9007199254740993}`), FinancialSnapshot: json.RawMessage(`{"wallet_id":"22222222-2222-4222-8222-222222222222","delta_microcredits":9223372036854775807}`),
		}},
		{"provider-unicode", LegacyProofInput{
			CommandType: "PAYMENT_PROVIDER_RESUME", PolicyVersion: 2, SnapshotSchemaVersion: 1, TargetResourceType: "PAYMENT_PROVIDER_CONTROL", TargetResourceID: "provider-main",
			NormalizedCommand:        json.RawMessage("{\"provider_id\":\"provider-main\",\"provider_version\":\"pv<>&/  😀é\\u0001\",\"provider_control_version\":9007199254740993,\"probe_order_id\":\"44444444-4444-4444-8444-444444444444\",\"policy_version\":\"payment-stop-loss/v1\",\"observation_high_watermark\":null,\"evaluation_high_watermark\":\"55555555-5555-4555-8555-555555555555\",\"active_mapping_set_fingerprint\":\"" + strings.Repeat("a", 32) + "\"}"),
			CriticalResourceVersions: json.RawMessage("{\"provider_id\":\"provider-main\",\"provider_version\":\"pv<>&/  😀é\\u0001\",\"provider_control_version\":9007199254740993,\"probe_order_id\":\"44444444-4444-4444-8444-444444444444\",\"policy_version\":\"payment-stop-loss/v1\",\"observation_high_watermark\":null,\"evaluation_high_watermark\":\"55555555-5555-4555-8555-555555555555\",\"active_mapping_set_fingerprint\":\"" + strings.Repeat("a", 32) + "\"}"),
			FinancialSnapshot:        json.RawMessage(`{"new_orders_enabled":false,"provider_control_version":9007199254740993,"product_control_version":null}`),
		}},
		{"product-versions", LegacyProofInput{
			CommandType: "PAYMENT_PRODUCT_RESUME", PolicyVersion: 1, SnapshotSchemaVersion: 1, TargetResourceType: "PAYMENT_PRODUCT_CONTROL", TargetResourceID: "product-main",
			NormalizedCommand:        json.RawMessage(`{"provider_id":"provider-main","provider_version":"provider-v1","product_id":"product-main","provider_control_version":7,"product_control_version":8,"catalog_item_id":"66666666-6666-4666-8666-666666666666","catalog_version":9,"admission_version":10,"mapping_id":"77777777-7777-4777-8777-777777777777","mapping_version":11,"probe_order_id":"88888888-8888-4888-8888-888888888888","policy_version":"payment-stop-loss/v1","observation_high_watermark":null,"evaluation_high_watermark":null}`),
			CriticalResourceVersions: json.RawMessage(`{"provider_id":"provider-main","provider_version":"provider-v1","product_id":"product-main","provider_control_version":7,"product_control_version":8,"catalog_item_id":"66666666-6666-4666-8666-666666666666","catalog_version":9,"admission_version":10,"mapping_id":"77777777-7777-4777-8777-777777777777","mapping_version":11,"probe_order_id":"88888888-8888-4888-8888-888888888888","policy_version":"payment-stop-loss/v1","observation_high_watermark":null,"evaluation_high_watermark":null}`),
			FinancialSnapshot:        json.RawMessage(`{"new_orders_enabled":false,"provider_control_version":7,"product_control_version":8}`),
		}},
	}
}

func TestFrozenGoldenVectors(t *testing.T) {
	body, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture goldenFile
	if err := json.Unmarshal(body, &fixture); err != nil || fixture.SchemaVersion != 1 {
		t.Fatalf("invalid fixture: %v", err)
	}
	inputs := frozenLegacyInputs()
	if len(fixture.ProofVectors) != len(inputs) {
		t.Fatalf("proof vector count=%d want=%d", len(fixture.ProofVectors), len(inputs))
	}
	for index, item := range inputs {
		vector := fixture.ProofVectors[index]
		artifacts, err := ConvertLegacyProofInput(item.input)
		if err != nil {
			t.Fatalf("%s: %v", item.name, err)
		}
		if vector.Name != item.name || vector.CommandType != item.input.CommandType || vector.PolicyVersion != item.input.PolicyVersion || vector.SnapshotSchemaVersion != item.input.SnapshotSchemaVersion || vector.Legacy.NormalizedJSON != string(item.input.NormalizedCommand) || vector.Legacy.CriticalJSON != string(item.input.CriticalResourceVersions) || vector.Legacy.FinancialJSON != string(item.input.FinancialSnapshot) || vector.Legacy.TargetResourceType != item.input.TargetResourceType || vector.Legacy.TargetResourceID != item.input.TargetResourceID {
			t.Fatalf("%s: frozen input changed", item.name)
		}
		if vector.ProofJSON != string(artifacts.ProofInputJSON) || vector.CommandJCS != string(artifacts.CommandJCS) || vector.SnapshotJCS != string(artifacts.SnapshotJCS) || vector.CommandDigest != artifacts.CommandDigest || vector.SnapshotDigest != artifacts.SnapshotDigest {
			t.Fatalf("%s: canonical output changed", item.name)
		}
		decoded, err := DecodeProofInput(vector.CommandType, vector.PolicyVersion, []byte(vector.ProofJSON))
		if err != nil || decoded.SnapshotSchemaVersion != vector.SnapshotSchemaVersion || !bytes.Equal(decoded.ProofInputJSON, artifacts.ProofInputJSON) || !bytes.Equal(decoded.CommandJCS, artifacts.CommandJCS) || !bytes.Equal(decoded.SnapshotJCS, artifacts.SnapshotJCS) || decoded.CommandDigest != artifacts.CommandDigest || decoded.SnapshotDigest != artifacts.SnapshotDigest {
			t.Fatalf("%s: legacy/new semantic roundtrip changed: %v", item.name, err)
		}
	}
}

func TestLegacyExecutionInputIsClosedAndLossless(t *testing.T) {
	for _, item := range frozenLegacyInputs() {
		t.Run(item.name, func(t *testing.T) {
			proof, err := ConvertLegacyProofInput(item.input)
			if err != nil {
				t.Fatal(err)
			}
			var projection struct {
				Normalized json.RawMessage `json:"normalized_command"`
				Critical   json.RawMessage `json:"critical_resource_versions"`
				Financial  json.RawMessage `json:"financial_snapshot"`
			}
			if err := json.Unmarshal(proof.ProofInputJSON, &projection); err != nil {
				t.Fatal(err)
			}
			decoded := DecodedAuthorizationEvent{SerializationVersion: ApprovalSerializationVersion, Payload: AuthorizationEvent{
				CommandType: item.input.CommandType, PolicyVersion: item.input.PolicyVersion,
				TargetResourceType: item.input.TargetResourceType, TargetResourceID: item.input.TargetResourceID,
				NormalizedCommand: projection.Normalized, CriticalResourceVersions: projection.Critical, FinancialSnapshot: projection.Financial,
				CommandDigest: proof.CommandDigest, SnapshotDigest: proof.SnapshotDigest,
			}}
			execution, err := LegacyExecutionInput(decoded)
			if err != nil {
				t.Fatal(err)
			}
			roundTrip, err := ConvertLegacyProofInput(execution)
			if err != nil || roundTrip.CommandDigest != proof.CommandDigest || roundTrip.SnapshotDigest != proof.SnapshotDigest {
				t.Fatalf("execution projection changed proof facts: %v", err)
			}
			for _, raw := range []json.RawMessage{execution.NormalizedCommand, execution.CriticalResourceVersions, execution.FinancialSnapshot} {
				if bytes.Contains(raw, []byte(`"9223372036854775807"`)) || bytes.Contains(raw, []byte(`"9007199254740993"`)) {
					t.Fatal("int64 execution value remained a JSON string")
				}
			}
		})
	}

	proof, err := ConvertLegacyProofInput(frozenLegacyInputs()[0].input)
	if err != nil {
		t.Fatal(err)
	}
	var projection struct {
		Normalized json.RawMessage `json:"normalized_command"`
		Critical   json.RawMessage `json:"critical_resource_versions"`
		Financial  json.RawMessage `json:"financial_snapshot"`
	}
	if err := json.Unmarshal(proof.ProofInputJSON, &projection); err != nil {
		t.Fatal(err)
	}
	valid := DecodedAuthorizationEvent{SerializationVersion: ApprovalSerializationVersion, Payload: AuthorizationEvent{
		CommandType: "WALLET_ADJUSTMENT", PolicyVersion: 1, TargetResourceType: "MANUAL_ADJUSTMENT", TargetResourceID: "11111111-1111-4111-8111-111111111111",
		NormalizedCommand: projection.Normalized, CriticalResourceVersions: projection.Critical, FinancialSnapshot: projection.Financial,
		CommandDigest: proof.CommandDigest, SnapshotDigest: proof.SnapshotDigest,
	}}
	for name, mutate := range map[string]func(*DecodedAuthorizationEvent){
		"legacy input":    func(value *DecodedAuthorizationEvent) { value.SerializationVersion = LegacySerializationVersion },
		"unknown command": func(value *DecodedAuthorizationEvent) { value.Payload.CommandType = "UNKNOWN" },
		"extra field": func(value *DecodedAuthorizationEvent) {
			value.Payload.NormalizedCommand = append(bytes.TrimSuffix(value.Payload.NormalizedCommand, []byte("}")), []byte(`,"extra":true}`)...)
		},
		"overflow": func(value *DecodedAuthorizationEvent) {
			value.Payload.NormalizedCommand = bytes.Replace(value.Payload.NormalizedCommand, []byte(`"9223372036854775807"`), []byte(`"9223372036854775808"`), 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := valid
			changed.Payload.NormalizedCommand = bytes.Clone(valid.Payload.NormalizedCommand)
			changed.Payload.CriticalResourceVersions = bytes.Clone(valid.Payload.CriticalResourceVersions)
			changed.Payload.FinancialSnapshot = bytes.Clone(valid.Payload.FinancialSnapshot)
			mutate(&changed)
			if _, err := LegacyExecutionInput(changed); err == nil {
				t.Fatal("invalid execution projection was accepted")
			}
		})
	}
}

func TestAuthorizationWireAndProofResponseGolden(t *testing.T) {
	fixture := readGolden(t)
	if len(fixture.AuthorizationVectors) != 2 || len(fixture.ProofResponseVectors) != 1 {
		t.Fatal("authorization/proof-response golden matrix incomplete")
	}
	for _, vector := range fixture.AuthorizationVectors {
		raw := []byte(vector.EventJSON)
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != vector.EventDigest {
			t.Fatalf("%s digest changed", vector.Name)
		}
		expectedVersion, err := parseFrozenInt64(vector.ApprovedRequestVersion)
		if err != nil {
			t.Fatal(err)
		}
		expectedEpoch, err := parseFrozenInt64(vector.ApprovalEpoch)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodeAuthorizationEvent(raw, ExpectedAuthorizationBinding{ApprovedRequestVersion: expectedVersion, ApprovalEpoch: expectedEpoch})
		if err != nil || decoded.SerializationVersion != vector.SerializationVersion {
			t.Fatalf("%s decode: %v", vector.Name, err)
		}
		if vector.SerializationVersion == ApprovalSerializationVersion {
			reencoded, err := EncodeAuthorizationEvent(decoded.Payload, ExpectedAuthorizationBinding{ApprovedRequestVersion: expectedVersion, ApprovalEpoch: expectedEpoch})
			if err != nil || !bytes.Equal(reencoded, raw) {
				t.Fatalf("%s new wire roundtrip: %v", vector.Name, err)
			}
		}
	}
	generated := makeGoldenFile(t)
	if len(generated.AuthorizationVectors) != len(fixture.AuthorizationVectors) {
		t.Fatal("generated authorization vector count changed")
	}
	for index := range generated.AuthorizationVectors {
		if fixture.AuthorizationVectors[index] != generated.AuthorizationVectors[index] {
			t.Fatalf("%s: frozen Event.Marshal or proof-era authorization bytes changed", fixture.AuthorizationVectors[index].Name)
		}
	}
	if fixture.ProofResponseVectors[0] != generated.ProofResponseVectors[0] {
		t.Fatal("proof response canonical bytes or locally derived digest changed")
	}
}

func readGolden(t *testing.T) goldenFile {
	t.Helper()
	body, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture goldenFile
	if err := json.Unmarshal(body, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func parseFrozenInt64(value string) (int64, error) {
	node := &jsonValue{kind: jsonStringKind, text: value}
	return decimalString(node)
}

func makeGoldenFile(t *testing.T) goldenFile {
	t.Helper()
	result := goldenFile{SchemaVersion: 1}
	inputs := frozenLegacyInputs()
	for _, item := range inputs {
		artifacts, err := ConvertLegacyProofInput(item.input)
		if err != nil {
			t.Fatal(err)
		}
		result.ProofVectors = append(result.ProofVectors, goldenProofVector{
			Name: item.name, CommandType: item.input.CommandType, PolicyVersion: item.input.PolicyVersion, SnapshotSchemaVersion: item.input.SnapshotSchemaVersion,
			Legacy:    goldenLegacy{TargetResourceType: item.input.TargetResourceType, TargetResourceID: item.input.TargetResourceID, NormalizedJSON: string(item.input.NormalizedCommand), CriticalJSON: string(item.input.CriticalResourceVersions), FinancialJSON: string(item.input.FinancialSnapshot)},
			ProofJSON: string(artifacts.ProofInputJSON), CommandJCS: string(artifacts.CommandJCS), SnapshotJCS: string(artifacts.SnapshotJCS), CommandDigest: artifacts.CommandDigest, SnapshotDigest: artifacts.SnapshotDigest,
		})
	}
	requestID := uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc")
	legacyEvent := events.Event{ID: uuid.MustParse("dddddddd-dddd-4ddd-8ddd-dddddddddddd"), Type: AuthorizationEventType, AggregateID: requestID, AggregateVersion: 2, OccurredAt: time.Date(2026, 9, 15, 1, 2, 3, 456000000, time.UTC), TraceID: "trace-legacy<>&", Payload: mustLegacyPayload(t, inputs[1].input, requestID)}
	legacyBytes, err := EncodeLegacyAuthorizationEvent(legacyEvent)
	if err != nil {
		t.Fatal(err)
	}
	result.AuthorizationVectors = append(result.AuthorizationVectors, frozenAuthorization("legacy-provider", LegacySerializationVersion, legacyBytes, 2, 1))
	artifacts, err := ConvertLegacyProofInput(inputs[0].input)
	if err != nil {
		t.Fatal(err)
	}
	newEvent := AuthorizationEvent{
		EventID: uuid.MustParse("eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"), AggregateID: requestID, AggregateVersion: 3, OccurredAt: time.Date(2026, 9, 15, 1, 3, 4, 567000000, time.UTC), TraceID: "trace-new<>&/  😀é\u0001",
		ApprovalRequestID: requestID, CommandType: inputs[0].input.CommandType, CommandFingerprint: strings.Repeat("1", 64), PolicyVersion: 1, ExecutorIdentity: "billing-command-executor", ExecutionDeadline: time.Date(2026, 9, 15, 1, 8, 4, 567000000, time.UTC),
		NormalizedCommand: extractProofPart(t, artifacts.ProofInputJSON, "normalized_command"), TargetResourceType: inputs[0].input.TargetResourceType, TargetResourceID: inputs[0].input.TargetResourceID,
		CriticalResourceVersions: extractProofPart(t, artifacts.ProofInputJSON, "critical_resource_versions"), FinancialSnapshot: extractProofPart(t, artifacts.ProofInputJSON, "financial_snapshot"),
		ProofSchemaVersion: ProofSchemaVersion, CommandDigest: artifacts.CommandDigest, SnapshotDigest: artifacts.SnapshotDigest, DomainRevision: "wallet-revision-1", ProofDigest: strings.Repeat("2", 64), ApprovalControlEpoch: 9007199254740993,
	}
	newBytes, err := EncodeAuthorizationEvent(newEvent, ExpectedAuthorizationBinding{ApprovedRequestVersion: 3, ApprovalEpoch: 9007199254740993})
	if err != nil {
		t.Fatal(err)
	}
	result.AuthorizationVectors = append(result.AuthorizationVectors, frozenAuthorization("new-wallet", ApprovalSerializationVersion, newBytes, 3, 9007199254740993))
	response := ProofResponse{ApprovalRequestID: requestID, CommandType: "WALLET_ADJUSTMENT", CommandDigest: artifacts.CommandDigest, SnapshotDigest: artifacts.SnapshotDigest, PolicyVersion: 1, ProofSchemaVersion: ProofSchemaVersion, DomainRevision: "wallet-revision-1", Verdict: "ALLOW", ReasonCode: "PROOF_VALID", IssuedAt: time.Date(2026, 9, 15, 1, 4, 5, 678000000, time.UTC), ExpiresAt: time.Date(2026, 9, 15, 1, 5, 5, 678000000, time.UTC)}
	digest, err := DeriveProofDigest(response)
	if err != nil {
		t.Fatal(err)
	}
	responseNode := newObject(map[string]*jsonValue{"approval_request_id": jsonString(response.ApprovalRequestID.String()), "command_type": jsonString(response.CommandType), "command_digest": jsonString(response.CommandDigest), "snapshot_digest": jsonString(response.SnapshotDigest), "policy_version": jsonInteger(response.PolicyVersion), "proof_schema_version": jsonString(response.ProofSchemaVersion), "domain_revision": jsonString(response.DomainRevision), "verdict": jsonString(response.Verdict), "reason_code": jsonString(response.ReasonCode), "issued_at": jsonString(response.IssuedAt.Format(time.RFC3339Nano)), "expires_at": jsonString(response.ExpiresAt.Format(time.RFC3339Nano))})
	responseJSON, _ := canonicalJSON(responseNode)
	result.ProofResponseVectors = append(result.ProofResponseVectors, goldenProofResponseVector{Name: "wallet-allow", ResponseJSON: string(responseJSON), ProofDigest: hex.EncodeToString(digest[:])})
	return result
}

func frozenAuthorization(name, version string, raw []byte, requestVersion, epoch int64) goldenAuthorizationVector {
	sum := sha256.Sum256(raw)
	return goldenAuthorizationVector{Name: name, SerializationVersion: version, EventJSON: string(raw), EventDigest: hex.EncodeToString(sum[:]), ApprovedRequestVersion: int64Text(requestVersion), ApprovalEpoch: int64Text(epoch)}
}

func mustLegacyPayload(t *testing.T, input LegacyProofInput, requestID uuid.UUID) json.RawMessage {
	t.Helper()
	spec, ok := commandSpecs[input.CommandType]
	if !ok {
		t.Fatal("missing command spec")
	}
	payload := map[string]any{"approval_request_id": requestID, "command_type": input.CommandType, "command_fingerprint": strings.Repeat("3", 64), "policy_version": input.PolicyVersion, "executor_identity": spec.executor, "execution_deadline": time.Date(2026, 9, 15, 1, 7, 3, 456000000, time.UTC), "normalized_command": input.NormalizedCommand, "target_resource_type": input.TargetResourceType, "target_resource_id": input.TargetResourceID, "critical_resource_versions": input.CriticalResourceVersions, "financial_snapshot": input.FinancialSnapshot}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func extractProofPart(t *testing.T, raw []byte, key string) json.RawMessage {
	t.Helper()
	var value map[string]json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return value[key]
}
