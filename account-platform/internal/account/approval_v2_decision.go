package account

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
)

var errInvalidApprovalDecision = errors.New("invalid v2 approval decision")

type approvalV2Decision struct {
	ExpectedVersion int64
	Decision        string
	Reason          string
}

// decodeApprovalV2Decision rejects aliases, duplicate keys, unknown proof
// material and trailing JSON before invoking any repository/domain method.
func decodeApprovalV2Decision(body io.Reader) (approvalV2Decision, error) {
	const maxBody = 8192
	raw, err := io.ReadAll(io.LimitReader(body, maxBody+1))
	if err != nil || len(raw) > maxBody {
		return approvalV2Decision{}, errInvalidApprovalDecision
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return approvalV2Decision{}, errInvalidApprovalDecision
	}
	var result approvalV2Decision
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return approvalV2Decision{}, errInvalidApprovalDecision
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return approvalV2Decision{}, errInvalidApprovalDecision
		}
		seen[key] = true
		var field json.RawMessage
		if err := decoder.Decode(&field); err != nil {
			return approvalV2Decision{}, errInvalidApprovalDecision
		}
		switch key {
		case "expected_version":
			if len(field) == 0 || field[0] < '1' || field[0] > '9' {
				return approvalV2Decision{}, errInvalidApprovalDecision
			}
			result.ExpectedVersion, err = strconv.ParseInt(string(field), 10, 64)
		case "decision":
			err = json.Unmarshal(field, &result.Decision)
		case "reason":
			err = json.Unmarshal(field, &result.Reason)
		default:
			return approvalV2Decision{}, errInvalidApprovalDecision
		}
		if err != nil {
			return approvalV2Decision{}, errInvalidApprovalDecision
		}
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') || decoder.Decode(new(any)) != io.EOF || len(seen) != 3 ||
		result.ExpectedVersion <= 0 || result.Decision != strings.ToUpper(result.Decision) ||
		(result.Decision != "APPROVED" && result.Decision != "REJECTED") ||
		len(strings.TrimSpace(result.Reason)) < 3 || len(strings.TrimSpace(result.Reason)) > 2000 {
		return approvalV2Decision{}, errInvalidApprovalDecision
	}
	return result, nil
}
