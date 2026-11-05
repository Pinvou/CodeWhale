package account

import (
	"strings"
	"testing"
)

func TestApprovalV2DecisionClosedBody(t *testing.T) {
	good := `{"expected_version":9223372036854775807,"action_control_version":9223372036854775807,"decision":"APPROVED","reason":"independent approval"}`
	decision, err := decodeApprovalV2Decision(strings.NewReader(good))
	if err != nil || decision.ExpectedVersion != 9223372036854775807 || decision.ActionControlVersion != 9223372036854775807 || decision.Decision != "APPROVED" {
		t.Fatalf("exact int64 decision: %+v %v", decision, err)
	}
	for _, input := range []string{
		`{"expected_version":1,"decision":"APPROVED","reason":"okay"}`,
		`{"expected_version":1,"action_control_version":0,"decision":"APPROVED","reason":"okay"}`,
		`{"expected_version":1,"action_control_version":1.0,"decision":"APPROVED","reason":"okay"}`,
		`{"expected_version":1,"action_control_version":1,"action_control_version":1,"decision":"APPROVED","reason":"okay"}`,
		`{"expected_version":1,"decision":"APPROVED","reason":"okay","observed_versions":{}}`,
		`{"expected_version":1,"decision":"APPROVED","reason":"okay","proof":{}}`,
		`{"expected_version":1,"decision":"APPROVED","reason":"okay","reason":"duplicate"}`,
		`{"expected_version":1.0,"decision":"APPROVED","reason":"okay"}`,
		`{"expected_version":9223372036854775808,"decision":"APPROVED","reason":"okay"}`,
		`{"expected_version":1,"decision":"approved","reason":"okay"}`,
		`{"expected_version":1,"decision":"APPROVED","reason":"okay"} true`,
		strings.Repeat(" ", 8193) + good,
	} {
		if _, err := decodeApprovalV2Decision(strings.NewReader(input)); err == nil {
			t.Fatalf("accepted unclosed decision: %.110s", input)
		}
	}
}
