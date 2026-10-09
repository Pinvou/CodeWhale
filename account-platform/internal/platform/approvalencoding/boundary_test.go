package approvalencoding

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func boundaryWallet(amount string) LegacyProofInput {
	return LegacyProofInput{
		CommandType: "WALLET_ADJUSTMENT", PolicyVersion: 1, SnapshotSchemaVersion: 1,
		TargetResourceType: "MANUAL_ADJUSTMENT", TargetResourceID: "11111111-1111-4111-8111-111111111111",
		NormalizedCommand:        json.RawMessage(`{"adjustment_id":"11111111-1111-4111-8111-111111111111","wallet_id":"22222222-2222-4222-8222-222222222222","amount_microcredits":` + amount + `,"requested_by":"33333333-3333-4333-8333-333333333333"}`),
		CriticalResourceVersions: json.RawMessage(`{"adjustment_version":9223372036854775807}`),
		FinancialSnapshot:        json.RawMessage(`{"wallet_id":"22222222-2222-4222-8222-222222222222","delta_microcredits":` + amount + `}`),
	}
}

func TestIndependentAuthorizationEncodingBoundary(t *testing.T) {
	input := boundaryWallet("9223372036854775807")
	proof, err := ConvertLegacyProofInput(input)
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
	request := uuid.MustParse(input.TargetResourceID)
	expected := ExpectedAuthorizationBinding{ApprovedRequestVersion: 9223372036854775807, ApprovalEpoch: 9007199254740993}
	event := AuthorizationEvent{
		EventID: uuid.MustParse("44444444-4444-4444-8444-444444444444"), AggregateID: request,
		AggregateVersion: expected.ApprovedRequestVersion, ApprovalRequestID: request,
		OccurredAt: time.Date(2026, 9, 15, 0, 0, 0, 1, time.UTC), TraceID: "trace<>&/\u2028😀",
		CommandType: input.CommandType, PolicyVersion: input.PolicyVersion, CommandFingerprint: strings.Repeat("a", 64),
		ExecutorIdentity: "billing-command-executor", ExecutionDeadline: time.Date(2026, 9, 15, 0, 5, 0, 1, time.UTC),
		NormalizedCommand: projection.Normalized, CriticalResourceVersions: projection.Critical, FinancialSnapshot: projection.Financial,
		TargetResourceType: input.TargetResourceType, TargetResourceID: input.TargetResourceID,
		ProofSchemaVersion: ProofSchemaVersion, CommandDigest: proof.CommandDigest, SnapshotDigest: proof.SnapshotDigest,
		DomainRevision: "fixture-revision", ProofDigest: strings.Repeat("b", 64), ApprovalControlEpoch: expected.ApprovalEpoch,
	}
	raw, err := EncodeAuthorizationEvent(event, expected)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeAuthorizationEvent(raw, expected)
	if err != nil || decoded.AggregateVersion != expected.ApprovedRequestVersion || decoded.Payload.ApprovalControlEpoch != expected.ApprovalEpoch {
		t.Fatalf("authorization precision roundtrip failed: %v", err)
	}
	for name, change := range map[string]func(*AuthorizationEvent){
		"wrong version":     func(e *AuthorizationEvent) { e.AggregateVersion-- },
		"wrong epoch":       func(e *AuthorizationEvent) { e.ApprovalControlEpoch-- },
		"invalid Go UTF8":   func(e *AuthorizationEvent) { e.TraceID = string([]byte{0xff}) },
		"oversized trace":   func(e *AuthorizationEvent) { e.TraceID = strings.Repeat("t", MaximumDocumentBytes) },
		"wrong executor":    func(e *AuthorizationEvent) { e.ExecutorIdentity += "-v1" },
		"mismatched digest": func(e *AuthorizationEvent) { e.CommandDigest = strings.Repeat("c", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			mutated := event
			change(&mutated)
			if _, err := EncodeAuthorizationEvent(mutated, expected); err == nil {
				t.Fatal("invalid event encoded")
			}
		})
	}
}

func TestIndependentWalletPrecisionBoundary(t *testing.T) {
	for _, amount := range []string{"-9223372036854775808", "9223372036854775807", "9007199254740993", "-1", "1"} {
		t.Run(amount, func(t *testing.T) {
			input := boundaryWallet(amount)
			original := bytes.Clone(input.NormalizedCommand)
			artifacts, err := ConvertLegacyProofInput(input)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(original, input.NormalizedCommand) {
				t.Fatal("legacy input mutated")
			}
			if !bytes.Contains(artifacts.ProofInputJSON, []byte(`"amount_microcredits":"`+amount+`"`)) ||
				!bytes.Contains(artifacts.ProofInputJSON, []byte(`"adjustment_version":"9223372036854775807"`)) {
				t.Fatal("canonical projection lost integer precision or fixed string type")
			}
			decoded, err := DecodeProofInput(input.CommandType, input.PolicyVersion, artifacts.ProofInputJSON)
			if err != nil || !bytes.Equal(decoded.ProofInputJSON, artifacts.ProofInputJSON) || decoded.CommandDigest != artifacts.CommandDigest || decoded.SnapshotDigest != artifacts.SnapshotDigest {
				t.Fatalf("legacy/new semantic roundtrip differs: %v", err)
			}
		})
	}
}

func TestIndependentWalletRejectedLegacyNumbers(t *testing.T) {
	for _, amount := range []string{"0", "-0", "9223372036854775808", "-9223372036854775809", "1.0", "1e0", `"1"`, "null", "true", "01", "+1"} {
		t.Run(amount, func(t *testing.T) {
			if _, err := ConvertLegacyProofInput(boundaryWallet(amount)); err == nil {
				t.Fatal("invalid legacy amount accepted")
			}
		})
	}
}

func TestIndependentProofClosedBoundary(t *testing.T) {
	valid, err := ConvertLegacyProofInput(boundaryWallet("1"))
	if err != nil {
		t.Fatal(err)
	}
	raw := string(valid.ProofInputJSON)
	cases := map[string]string{
		"numeric instead of string": strings.Replace(raw, `"amount_microcredits":"1"`, `"amount_microcredits":1`, 1),
		"leading zero":              strings.Replace(raw, `"amount_microcredits":"1"`, `"amount_microcredits":"01"`, 1),
		"duplicate key":             strings.Replace(raw, `"amount_microcredits":"1"`, `"amount_microcredits":"1","amount_microcredits":"1"`, 1),
		"case alias":                strings.Replace(raw, `"amount_microcredits"`, `"Amount_microcredits"`, 1),
		"unknown sensitive field":   strings.Replace(raw, `"amount_microcredits":"1"`, `"amount_microcredits":"1","secret":"fixture"`, 1),
		"missing amount":            strings.Replace(raw, `"amount_microcredits":"1",`, "", 1),
		"financial mismatch":        strings.Replace(raw, `"delta_microcredits":"1"`, `"delta_microcredits":"2"`, 1),
		"version overflow":          strings.Replace(raw, `"adjustment_version":"9223372036854775807"`, `"adjustment_version":"9223372036854775808"`, 1),
		"trailing document":         raw + `{}`,
		"oversized":                 raw + strings.Repeat(" ", MaximumDocumentBytes+1-len(raw)),
		"lone surrogate":            strings.Replace(raw, "MANUAL_ADJUSTMENT", `\ud800`, 1),
		"invalid UTF8":              strings.Replace(raw, "MANUAL_ADJUSTMENT", string([]byte{0xff}), 1),
		"requester not UUID":        strings.Replace(raw, "33333333-3333-4333-8333-333333333333", "arbitrary-user", 1),
	}
	for name, invalid := range cases {
		t.Run(name, func(t *testing.T) {
			if invalid == raw {
				t.Fatal("negative test failed to change its fixture")
			}
			if _, err := DecodeProofInput("WALLET_ADJUSTMENT", 1, []byte(invalid)); err == nil {
				t.Fatal("invalid proof accepted")
			}
		})
	}
}
