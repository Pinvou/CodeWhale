package approvalencoding

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func lifecycleInput(version int64, force bool) LegacyProofInput {
	n := map[string]any{"model_id": "11111111-1111-4111-8111-111111111111", "lifecycle_plan_id": "22222222-2222-4222-8222-222222222222", "lifecycle_plan_version": 2, "target_lifecycle": "REMOVED", "effective_at": "2026-11-01T12:00:00Z", "existing_session_cutoff_at": "2026-11-01T12:00:00Z", "replacement_model_id": "33333333-3333-4333-8333-333333333333"}
	c := map[string]any{"model_id": n["model_id"], "catalog_version": 4, "current_lifecycle": "PUBLISHED", "target_lifecycle": n["target_lifecycle"], "lifecycle_plan_version": n["lifecycle_plan_version"], "effective_at": n["effective_at"], "existing_session_cutoff_at": n["existing_session_cutoff_at"], "replacement_model_id": n["replacement_model_id"], "billing_price_version": 5, "route_set_fingerprint": strings.Repeat("a", 64), "provisioning_set_fingerprint": strings.Repeat("b", 64), "inflight_request_set_fingerprint": strings.Repeat("c", 64), "policy_version": version}
	if version == 3 {
		n["force_cancel_inflight"] = force
		c["force_cancel_inflight"] = force
	}
	nr, _ := json.Marshal(n)
	cr, _ := json.Marshal(c)
	return LegacyProofInput{CommandType: "TOKEN_MODEL_LIFECYCLE_CHANGE", PolicyVersion: version, SnapshotSchemaVersion: int(version), TargetResourceType: "MODEL", TargetResourceID: n["model_id"].(string), NormalizedCommand: nr, CriticalResourceVersions: cr, FinancialSnapshot: json.RawMessage(`{"billing_price_revision":5,"price_set_fingerprint":"` + strings.Repeat("d", 64) + `","models_checked":2,"models_covered":2,"fully_covered":true,"billing_as_of":"2026-11-01T11:58:00Z"}`)}
}

func TestLifecycleV2ByteCompatibility(t *testing.T) {
	a, err := ConvertLegacyProofInput(lifecycleInput(2, false))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := os.ReadFile("testdata/lifecycle-v2-baseline.json")
	if err != nil {
		t.Fatal(err)
	}
	// This JSON container may use the checkout's line endings. Its byte slices
	// are base64 encoded, so decoding preserves every frozen protocol byte.
	var expected ProofArtifacts
	if err := json.Unmarshal(frozen, &expected); err != nil {
		t.Fatal(err)
	}
	frozen, err = json.MarshalIndent(expected, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, frozen) {
		t.Fatal("v2 canonical bytes or digests changed")
	}
}

func TestLifecycleVersionSelectionRetainsErrorsAndDefaultSpec(t *testing.T) {
	input := lifecycleInput(2, false)
	input.PolicyVersion = 4
	if _, err := ConvertLegacyProofInput(input); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("known unsupported version: %v", err)
	}
	input.CommandType = "UNKNOWN"
	if _, err := ConvertLegacyProofInput(input); !errors.Is(err, ErrUnsupportedCommand) {
		t.Fatalf("unknown command: %v", err)
	}
	v3, _ := commandSpecForVersion("TOKEN_MODEL_LIFECYCLE_CHANGE", 3)
	v3.normalizedKeys[0] = "changed-local-copy"
	v3.criticalKeys[0] = "changed-local-copy"
	if current := commandSpecs["TOKEN_MODEL_LIFECYCLE_CHANGE"]; current.policyVersion != 2 || current.snapshotVersion != 2 || current.normalizedKeys[0] != "model_id" || current.criticalKeys[0] != "model_id" {
		t.Fatal("explicit version mutated the default spec")
	}
}

func mutateLifecycle(input LegacyProofInput, part, key string, value any, remove bool) LegacyProofInput {
	var raw *json.RawMessage
	switch part {
	case "n":
		raw = &input.NormalizedCommand
	case "c":
		raw = &input.CriticalResourceVersions
	case "f":
		raw = &input.FinancialSnapshot
	}
	var object map[string]any
	_ = json.Unmarshal(*raw, &object)
	if remove {
		delete(object, key)
	} else {
		object[key] = value
	}
	*raw, _ = json.Marshal(object)
	return input
}

func TestLifecycleV3StrictApprovalFacts(t *testing.T) {
	for _, force := range []bool{false, true} {
		input := lifecycleInput(3, force)
		a, err := ConvertLegacyProofInput(input)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodeProofInput(input.CommandType, 3, a.ProofInputJSON)
		if err != nil || !bytes.Equal(a.ProofInputJSON, decoded.ProofInputJSON) {
			t.Fatalf("proof roundtrip: %v", err)
		}
		legacy, err := LegacyProofInputFromArtifacts(a)
		if err != nil || !bytes.Equal(legacy.NormalizedCommand, input.NormalizedCommand) || !bytes.Equal(legacy.CriticalResourceVersions, input.CriticalResourceVersions) {
			t.Fatalf("legacy roundtrip: %v", err)
		}
	}
	base := lifecycleInput(3, true)
	for _, part := range []string{"n", "c"} {
		for _, bad := range []any{nil, "true", 1, []any{true}, map[string]any{"value": true}} {
			if _, err := ConvertLegacyProofInput(mutateLifecycle(base, part, "force_cancel_inflight", bad, false)); err == nil {
				t.Fatalf("invalid flag accepted in %s: %#v", part, bad)
			}
		}
		if _, err := ConvertLegacyProofInput(mutateLifecycle(base, part, "force_cancel_inflight", nil, true)); err == nil {
			t.Fatal("missing flag accepted")
		}
		if _, err := ConvertLegacyProofInput(mutateLifecycle(base, part, "unregistered", false, false)); err == nil {
			t.Fatal("unknown field accepted")
		}
	}
	cases := []LegacyProofInput{
		mutateLifecycle(base, "c", "force_cancel_inflight", false, false),
		mutateLifecycle(base, "c", "policy_version", 2, false),
		mutateLifecycle(base, "c", "model_id", "44444444-4444-4444-8444-444444444444", false),
		mutateLifecycle(base, "c", "lifecycle_plan_version", 3, false),
		mutateLifecycle(base, "c", "effective_at", "2026-11-01T12:01:00Z", false),
		mutateLifecycle(base, "n", "lifecycle_plan_id", nil, false),
		mutateLifecycle(base, "n", "replacement_model_id", base.TargetResourceID, false),
		mutateLifecycle(base, "f", "fully_covered", false, false),
		mutateLifecycle(base, "f", "billing_price_revision", 6, false),
		mutateLifecycle(base, "c", "route_set_fingerprint", "invalid", false),
	}
	for i, input := range cases {
		if _, err := ConvertLegacyProofInput(input); err == nil {
			t.Fatalf("inconsistent case %d accepted", i)
		}
	}
	for _, versions := range [][2]int{{3, 2}, {2, 3}, {4, 4}} {
		input := base
		input.PolicyVersion = int64(versions[0])
		input.SnapshotSchemaVersion = versions[1]
		if _, err := ConvertLegacyProofInput(input); err == nil {
			t.Fatal("unsupported version accepted")
		}
	}
	for _, flag := range []bool{false, true} {
		v2 := mutateLifecycle(lifecycleInput(2, false), "n", "force_cancel_inflight", flag, false)
		if _, err := ConvertLegacyProofInput(v2); err == nil {
			t.Fatal("v2 accepted v3 field")
		}
	}
	// A deprecated plan can carry explicit false; true remains REMOVED-only.
	dep := base
	for _, part := range []string{"n", "c"} {
		dep = mutateLifecycle(dep, part, "target_lifecycle", "DEPRECATED", false)
		dep = mutateLifecycle(dep, part, "replacement_model_id", nil, false)
		dep = mutateLifecycle(dep, part, "existing_session_cutoff_at", nil, false)
	}
	if _, err := ConvertLegacyProofInput(dep); err == nil {
		t.Fatal("DEPRECATED true accepted")
	}
	for _, part := range []string{"n", "c"} {
		dep = mutateLifecycle(dep, part, "force_cancel_inflight", false, false)
	}
	if _, err := ConvertLegacyProofInput(dep); err != nil {
		t.Fatalf("DEPRECATED false rejected: %v", err)
	}
	for _, frozen := range frozenLegacyInputs() {
		input := mutateLifecycle(frozen.input, "n", "force_cancel_inflight", true, false)
		if _, err := ConvertLegacyProofInput(input); err == nil {
			t.Fatalf("other command relaxed: %s", frozen.name)
		}
	}
}

func TestLifecycleV3AuthorizationTransport(t *testing.T) {
	input := lifecycleInput(3, true)
	a, err := ConvertLegacyProofInput(input)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 1, 11, 58, 0, 0, time.UTC)
	response := ProofResponse{ApprovalRequestID: uuid.MustParse("44444444-4444-4444-8444-444444444444"), CommandType: input.CommandType, CommandDigest: a.CommandDigest, SnapshotDigest: a.SnapshotDigest, PolicyVersion: 3, ProofSchemaVersion: ProofSchemaVersion, DomainRevision: "1:" + strings.Repeat("a", 64), Verdict: "ALLOW", ReasonCode: "PROOF_VALID", IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
	digest, err := DeriveProofDigest(response)
	if err != nil {
		t.Fatal(err)
	}
	e := AuthorizationEvent{EventID: uuid.New(), AggregateID: response.ApprovalRequestID, AggregateVersion: 2, OccurredAt: now, TraceID: "lifecycle-v3-prototype", ApprovalRequestID: response.ApprovalRequestID, CommandType: input.CommandType, CommandFingerprint: strings.Repeat("b", 64), PolicyVersion: 3, ExecutorIdentity: "token-command-executor", ExecutionDeadline: now.Add(5 * time.Minute), NormalizedCommand: extractProofPart(t, a.ProofInputJSON, "normalized_command"), CriticalResourceVersions: extractProofPart(t, a.ProofInputJSON, "critical_resource_versions"), FinancialSnapshot: extractProofPart(t, a.ProofInputJSON, "financial_snapshot"), TargetResourceType: input.TargetResourceType, TargetResourceID: input.TargetResourceID, ProofSchemaVersion: ProofSchemaVersion, CommandDigest: a.CommandDigest, SnapshotDigest: a.SnapshotDigest, DomainRevision: response.DomainRevision, ProofDigest: hex.EncodeToString(digest[:]), ApprovalControlEpoch: 7}
	binding := ExpectedAuthorizationBinding{ApprovedRequestVersion: 2, ApprovalEpoch: 7}
	raw, err := EncodeAuthorizationEvent(e, binding)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeAuthorizationEvent(raw, binding)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := LegacyExecutionInput(decoded)
	if err != nil || !bytes.Equal(legacy.NormalizedCommand, input.NormalizedCommand) || legacy.SnapshotSchemaVersion != 3 {
		t.Fatalf("event domain roundtrip: %v", err)
	}
	tampered := bytes.Replace(raw, []byte(`"force_cancel_inflight":true`), []byte(`"force_cancel_inflight":false`), 1)
	if _, err := DecodeAuthorizationEvent(tampered, binding); err == nil {
		t.Fatal("tampered flag accepted")
	}
	for _, version := range []int64{2, 4} {
		changed := e
		changed.PolicyVersion = version
		if _, err := EncodeAuthorizationEvent(changed, binding); err == nil {
			t.Fatal("wrong event version accepted")
		}
	}
}
