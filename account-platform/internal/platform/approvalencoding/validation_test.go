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

func TestEntityUUIDAndBusinessTimeRejectZeroValues(t *testing.T) {
	wallet := frozenLegacyInputs()[0].input
	nilID := uuid.Nil.String()
	wallet.TargetResourceID = nilID
	wallet.NormalizedCommand = replaceAllJSON(wallet.NormalizedCommand, "11111111-1111-4111-8111-111111111111", nilID)
	if _, err := ConvertLegacyProofInput(wallet); err == nil {
		t.Fatal("nil target and adjustment UUID accepted")
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

}

func replaceAllJSON(raw json.RawMessage, old, replacement string) json.RawMessage {
	current := string(raw)
	if !strings.Contains(current, old) {
		panic("test mutation did not match its frozen input")
	}
	return json.RawMessage(strings.ReplaceAll(current, old, replacement))
}
