package token

import (
	"encoding/json"

	"testing"
)

func modelAccountEnvelope(t *testing.T, commandType, resourceType, resourceID string, command json.RawMessage, reason string, precheck TokenCommandPrecheckResult) TokenCommandEnvelope {
	t.Helper()
	var expected map[string]any
	if json.Unmarshal(precheck.ExpectedVersions, &expected) != nil {
		t.Fatal("precheck expected versions are invalid")
	}
	expected["precheck_fingerprint"] = precheck.PrecheckFingerprint
	envelope := TokenCommandEnvelope{CommandType: commandType, Reason: reason, TargetResourceType: resourceType, TargetResourceID: resourceID, PrecheckFingerprint: precheck.PrecheckFingerprint, Command: command}
	expected["request_fingerprint"] = governedRequestFingerprint(envelope, expected)
	envelope.ExpectedVersions = mustMarshalJSON(t, expected)
	return envelope
}
