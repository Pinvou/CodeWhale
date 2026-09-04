package token

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRuntimeGovernedCommandParserIsClosedAndNeverReplaysModelRequest(t *testing.T) {
	id := uuid.New()
	version := time.Now().UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
	provisioning := `{"provisioning_id":"` + id.String() + `","expected_provisioning_version":"` + version + `","target_generation":3,"replay_model_request":false}`
	usage := `{"usage_request_id":"` + id.String() + `","expected_usage_version":"` + version + `","evidence_high_watermark":9,"replay_model_request":false}`
	if parsed, err := parseRuntimeGovernedCommand("TOKEN_ROUTE_PROVISIONING_REPAIR", id.String(), json.RawMessage(provisioning)); err != nil || parsed.ResourceID != id || parsed.TargetGeneration != 3 {
		t.Fatalf("provisioning parse=%+v err=%v", parsed, err)
	}
	if parsed, err := parseRuntimeGovernedCommand("TOKEN_USAGE_RECONCILIATION_RETRY", id.String(), json.RawMessage(usage)); err != nil || parsed.ResourceID != id || parsed.EvidenceHighWatermark != 9 {
		t.Fatalf("usage parse=%+v err=%v", parsed, err)
	}

	invalid := []struct {
		name        string
		commandType string
		target      string
		body        string
	}{
		{"missing replay", "TOKEN_ROUTE_PROVISIONING_REPAIR", id.String(), `{"provisioning_id":"` + id.String() + `","expected_provisioning_version":"` + version + `","target_generation":3}`},
		{"true replay", "TOKEN_ROUTE_PROVISIONING_REPAIR", id.String(), `{"provisioning_id":"` + id.String() + `","expected_provisioning_version":"` + version + `","target_generation":3,"replay_model_request":true}`},
		{"string replay", "TOKEN_USAGE_RECONCILIATION_RETRY", id.String(), `{"usage_request_id":"` + id.String() + `","expected_usage_version":"` + version + `","evidence_high_watermark":9,"replay_model_request":"false"}`},
		{"extra field", "TOKEN_USAGE_RECONCILIATION_RETRY", id.String(), usage[:len(usage)-1] + `,"retry_upstream":true}`},
		{"wrong target", "TOKEN_ROUTE_PROVISIONING_REPAIR", uuid.NewString(), provisioning},
		{"non canonical UUID", "TOKEN_ROUTE_PROVISIONING_REPAIR", id.String(), `{"provisioning_id":"` + stringsToUpper(id.String()) + `","expected_provisioning_version":"` + version + `","target_generation":3,"replay_model_request":false}`},
		{"non canonical time", "TOKEN_USAGE_RECONCILIATION_RETRY", id.String(), `{"usage_request_id":"` + id.String() + `","expected_usage_version":"` + version + `+00:00","evidence_high_watermark":9,"replay_model_request":false}`},
		{"negative watermark", "TOKEN_USAGE_RECONCILIATION_RETRY", id.String(), `{"usage_request_id":"` + id.String() + `","expected_usage_version":"` + version + `","evidence_high_watermark":-1,"replay_model_request":false}`},
	}
	for _, testCase := range invalid {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := parseRuntimeGovernedCommand(testCase.commandType, testCase.target, json.RawMessage(testCase.body)); err == nil {
				t.Fatal("invalid runtime command was accepted")
			}
		})
	}
}

func stringsToUpper(value string) string {
	result := []byte(value)
	for index, current := range result {
		if current >= 'a' && current <= 'f' {
			result[index] = current - ('a' - 'A')
		}
	}
	return string(result)
}
