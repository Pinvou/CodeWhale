package token

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/tokengovernance"
)

type approvalActivationStub struct {
	state string
	epoch int64
}

func (r *approvalActivationStub) Snapshot(context.Context) (tokengovernance.Snapshot, error) {
	return tokengovernance.Snapshot{State: r.state, Epoch: r.epoch}, nil
}

func TestTokenApprovalJSONComparisonPreservesIntegerVersions(t *testing.T) {
	left := json.RawMessage(`{"version":9007199254740992,"nested":{"last":9223372036854775807}}`)
	if !equalTokenApprovalJSONObjects(left, json.RawMessage(` { "nested": {"last":9223372036854775807}, "version":9007199254740992 } `)) {
		t.Fatal("equivalent objects with large integer versions were rejected")
	}
	for _, right := range []json.RawMessage{
		json.RawMessage(`{"version":9007199254740993,"nested":{"last":9223372036854775807}}`),
		json.RawMessage(`{"version":9007199254740992,"nested":{"last":9223372036854775806}}`),
	} {
		if equalTokenApprovalJSONObjects(left, right) {
			t.Fatal("different exact integer versions compared equal")
		}
	}
	for _, invalid := range []json.RawMessage{json.RawMessage(`null`), json.RawMessage(`[]`), json.RawMessage(`{} {}`), json.RawMessage(`{} garbage`)} {
		if equalTokenApprovalJSONObjects(invalid, invalid) || equalTokenApprovalJSONObjects(left, invalid) {
			t.Fatal("non-single-object input compared equal")
		}
	}
}

func TestApprovalTransportRejectsDuplicateAndUnknownFields(t *testing.T) {
	for _, raw := range []string{`{"state":"ACTIVE","state":"SAFE"}`, `{"unknown":true}`, `{"normalized_command":{"id":1,"id":2}}`, `{} {}`} {
		var value tokenAuthorizedEvent
		if decodeApprovalTransport([]byte(raw), &value) == nil {
			t.Fatalf("ambiguous transport accepted: %s", raw)
		}
	}
	type claim struct {
		FirstClaim bool `json:"first_claim"`
	}
	var value claim
	if err := decodeApprovalTransport([]byte(`{"first_claim":false}`), &value); err != nil || value.FirstClaim {
		t.Fatalf("valid false claim rejected: %v", err)
	}
	for _, raw := range []string{`{"first_claim":false,"First_Claim":true}`, `{"First_Claim":true}`, `{"first_claim":null}`, `{}`, `{"first_claim":false,"first_claim":true}`} {
		if decodeApprovalTransport([]byte(raw), &value) == nil {
			t.Fatalf("claim authority ambiguity accepted: %s", raw)
		}
	}
}
