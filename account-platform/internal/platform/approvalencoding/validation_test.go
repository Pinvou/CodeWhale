package approvalencoding

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
)

func TestProviderLegacyFingerprintCompatibility(t *testing.T) {
	input := frozenLegacyInputs()[1].input
	if _, err := ConvertLegacyProofInput(input); err != nil {
		t.Fatalf("production-compatible 32-character MD5 fact rejected: %v", err)
	}
	input.NormalizedCommand = json.RawMessage(strings.Replace(string(input.NormalizedCommand), strings.Repeat("a", 32), strings.Repeat("a", 64), 1))
	input.CriticalResourceVersions = json.RawMessage(strings.Replace(string(input.CriticalResourceVersions), strings.Repeat("a", 32), strings.Repeat("a", 64), 1))
	if _, err := ConvertLegacyProofInput(input); err == nil {
		t.Fatal("provider p2 accepted a substituted 64-character mapping fingerprint")
	}
}

func TestLifecycleExistingQualificationRules(t *testing.T) {
	base := frozenLegacyInputs()[3].input
	modelID := base.TargetResourceID
	replacementID := "aaaaaaaa-1111-4111-8111-111111111111"
	cases := map[string]func(*LegacyProofInput){
		"noncanonical negative zero plan": func(input *LegacyProofInput) {
			input.CriticalResourceVersions = replaceAllJSON(input.CriticalResourceVersions, `"lifecycle_plan_version":0`, `"lifecycle_plan_version":-0`)
		},
		"null effective": func(input *LegacyProofInput) {
			input.NormalizedCommand = replaceAllJSON(input.NormalizedCommand, `"effective_at":"2026-09-15T01:02:03Z"`, `"effective_at":null`)
			input.CriticalResourceVersions = replaceAllJSON(input.CriticalResourceVersions, `"effective_at":"2026-09-15T01:02:03Z"`, `"effective_at":null`)
		},
		"zero effective": func(input *LegacyProofInput) {
			input.NormalizedCommand = replaceAllJSON(input.NormalizedCommand, "2026-09-15T01:02:03Z", "0001-01-01T00:00:00Z")
			input.CriticalResourceVersions = replaceAllJSON(input.CriticalResourceVersions, "2026-09-15T01:02:03Z", "0001-01-01T00:00:00Z")
		},
		"self replacement": func(input *LegacyProofInput) {
			input.NormalizedCommand = replaceAllJSON(input.NormalizedCommand, `"replacement_model_id":null`, `"replacement_model_id":"`+modelID+`"`)
			input.CriticalResourceVersions = replaceAllJSON(input.CriticalResourceVersions, `"replacement_model_id":null`, `"replacement_model_id":"`+modelID+`"`)
		},
		"removed without replacement": func(input *LegacyProofInput) {
			input.NormalizedCommand = replaceAllJSON(input.NormalizedCommand, "DEPRECATED", "REMOVED")
			input.CriticalResourceVersions = replaceAllJSON(input.CriticalResourceVersions, "DEPRECATED", "REMOVED")
		},
		"deprecated cutoff before effective": func(input *LegacyProofInput) {
			input.NormalizedCommand = replaceAllJSON(input.NormalizedCommand, `"existing_session_cutoff_at":null`, `"existing_session_cutoff_at":"2026-09-15T01:02:02Z"`)
			input.CriticalResourceVersions = replaceAllJSON(input.CriticalResourceVersions, `"existing_session_cutoff_at":null`, `"existing_session_cutoff_at":"2026-09-15T01:02:02Z"`)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			input := base
			mutate(&input)
			if _, err := ConvertLegacyProofInput(input); err == nil {
				t.Fatal("invalid lifecycle fact accepted")
			}
		})
	}

	validRemoved := base
	validRemoved.NormalizedCommand = replaceAllJSON(validRemoved.NormalizedCommand, "DEPRECATED", "REMOVED")
	validRemoved.CriticalResourceVersions = replaceAllJSON(validRemoved.CriticalResourceVersions, "DEPRECATED", "REMOVED")
	validRemoved.NormalizedCommand = replaceAllJSON(validRemoved.NormalizedCommand, `"existing_session_cutoff_at":null`, `"existing_session_cutoff_at":"2026-09-15T01:02:03Z"`)
	validRemoved.CriticalResourceVersions = replaceAllJSON(validRemoved.CriticalResourceVersions, `"existing_session_cutoff_at":null`, `"existing_session_cutoff_at":"2026-09-15T01:02:03Z"`)
	validRemoved.NormalizedCommand = replaceAllJSON(validRemoved.NormalizedCommand, `"replacement_model_id":null`, `"replacement_model_id":"`+replacementID+`"`)
	validRemoved.CriticalResourceVersions = replaceAllJSON(validRemoved.CriticalResourceVersions, `"replacement_model_id":null`, `"replacement_model_id":"`+replacementID+`"`)
	if _, err := ConvertLegacyProofInput(validRemoved); err != nil {
		t.Fatalf("valid REMOVED lifecycle rejected: %v", err)
	}
}

func TestEntityUUIDAndBusinessTimeRejectZeroValues(t *testing.T) {
	wallet := frozenLegacyInputs()[0].input
	nilID := uuid.Nil.String()
	wallet.TargetResourceID = nilID
	wallet.NormalizedCommand = replaceAllJSON(wallet.NormalizedCommand, "11111111-1111-4111-8111-111111111111", nilID)
	if _, err := ConvertLegacyProofInput(wallet); err == nil {
		t.Fatal("nil target and adjustment UUID accepted")
	}

	catalog := frozenLegacyInputs()[4].input
	catalog.FinancialSnapshot = replaceAllJSON(catalog.FinancialSnapshot, "2026-09-15T01:02:03Z", "0001-01-01T00:00:00Z")
	if _, err := ConvertLegacyProofInput(catalog); err == nil {
		t.Fatal("zero billing_as_of accepted")
	}
}

func TestCommandSpecificClosedRelationFailures(t *testing.T) {
	inputs := frozenLegacyInputs()
	cases := []struct {
		name   string
		input  LegacyProofInput
		mutate func(*LegacyProofInput)
	}{
		{
			name: "product financial version mismatch", input: inputs[2].input,
			mutate: func(input *LegacyProofInput) {
				input.FinancialSnapshot = replaceAllJSON(input.FinancialSnapshot, `"product_control_version":8`, `"product_control_version":9`)
			},
		},
		{
			name: "catalog source mismatch", input: inputs[4].input,
			mutate: func(input *LegacyProofInput) {
				input.CriticalResourceVersions = replaceAllJSON(input.CriticalResourceVersions, `"source_catalog_version":9223372036854775807`, `"source_catalog_version":9223372036854775806`)
			},
		},
		{
			name: "credential account version mismatch", input: inputs[5].input,
			mutate: func(input *LegacyProofInput) {
				input.CriticalResourceVersions = replaceAllJSON(input.CriticalResourceVersions, `"token_account_version":17`, `"token_account_version":19`)
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			test.mutate(&test.input)
			if _, err := ConvertLegacyProofInput(test.input); err == nil {
				t.Fatal("command-specific relation violation accepted")
			}
		})
	}
}

func TestLegacyAuthorizationRequiresEpochOne(t *testing.T) {
	input := frozenLegacyInputs()[0].input
	requestID := uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc")
	event := events.Event{
		ID: uuid.MustParse("dddddddd-dddd-4ddd-8ddd-dddddddddddd"), Type: AuthorizationEventType,
		AggregateID: requestID, AggregateVersion: 2,
		OccurredAt: time.Date(2026, 9, 15, 1, 2, 3, 456000000, time.UTC), TraceID: "trace-legacy",
		Payload: mustLegacyPayload(t, input, requestID),
	}
	raw, err := EncodeLegacyAuthorizationEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeAuthorizationEvent(raw, ExpectedAuthorizationBinding{ApprovedRequestVersion: 2, ApprovalEpoch: 1}); err != nil {
		t.Fatalf("legacy epoch 1 rejected: %v", err)
	}
	for _, epoch := range []int64{0, 2} {
		if _, err := DecodeAuthorizationEvent(raw, ExpectedAuthorizationBinding{ApprovedRequestVersion: 2, ApprovalEpoch: epoch}); err == nil {
			t.Fatalf("legacy epoch %d accepted", epoch)
		}
	}

	lifecycle := frozenLegacyInputs()[3].input
	event.Payload = mustLegacyPayload(t, lifecycle, requestID)
	lifecycleRaw, err := EncodeLegacyAuthorizationEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeAuthorizationEvent(lifecycleRaw, ExpectedAuthorizationBinding{ApprovedRequestVersion: 2, ApprovalEpoch: 1}); err != nil {
		t.Fatalf("valid legacy lifecycle rejected: %v", err)
	}
	lifecycle.CriticalResourceVersions = replaceAllJSON(lifecycle.CriticalResourceVersions, `"lifecycle_plan_version":0`, `"lifecycle_plan_version":-0`)
	event.Payload = mustLegacyPayload(t, lifecycle, requestID)
	lifecycleRaw, err = EncodeLegacyAuthorizationEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeAuthorizationEvent(lifecycleRaw, ExpectedAuthorizationBinding{ApprovedRequestVersion: 2, ApprovalEpoch: 1}); err == nil {
		t.Fatal("legacy authorization decoder normalized lifecycle -0 into 0")
	}
}

func replaceAllJSON(raw json.RawMessage, old, replacement string) json.RawMessage {
	current := string(raw)
	if !strings.Contains(current, old) {
		panic("test mutation did not match its frozen input")
	}
	return json.RawMessage(strings.ReplaceAll(current, old, replacement))
}
