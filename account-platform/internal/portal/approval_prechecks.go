package portal

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
)

// maximumApprovalPrecheckBytes mirrors the Account/billing closed document
// bound: the BFF never widens the transport the owner domain already enforces.
const maximumApprovalPrecheckBytes = 64 << 10

// approvalPrecheckBFFKeys is the closed key set the business page may send.
// Anything else is rejected before the Account service is called, so a page bug
// cannot smuggle an extra fact into the owner read.
var approvalPrecheckBFFKeys = map[string]bool{
	"command_type": true, "target_resource_id": true, "target_journal_entry_id": true,
	"payment_evidence_id": true, "anomaly_case_id": true, "usage_request_id": true,
	"loss_review_id": true, "business_date": true, "reason_code": true,
	"expected_version": true, "write_off_microcredits": true, "wrong_ledger_account_id": true,
	"correct_ledger_account_id": true, "amount_microcredits": true,
}

// adminApprovalPrechecks is the read-only business-page surface. It authenticates
// the server session, requires the CSRF token (the answer leaks owner facts), and
// forwards the closed input to the Account precheck proxy with the user's own
// access token, so the Account service still performs the real permission check.
func (s *Service) adminApprovalPrechecks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_PRECHECK_REQUEST", "the precheck accepts no query", httpserver.RequestID(r.Context()))
		return
	}
	for key := range r.Header {
		if strings.HasPrefix(strings.ToLower(key), "x-pinvou-approval-proof-") {
			api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_PRECHECK_REQUEST", "browser proof is not accepted", httpserver.RequestID(r.Context()))
			return
		}
	}
	session, err := s.authenticatedSession(r.Context(), r)
	if err != nil {
		api.WriteError(w, http.StatusUnauthorized, "LOGIN_REQUIRED", "login is required", httpserver.RequestID(r.Context()))
		return
	}
	if !s.validMutation(r, session) {
		api.WriteError(w, http.StatusForbidden, "CSRF_REJECTED", "the request origin or CSRF token is invalid", httpserver.RequestID(r.Context()))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maximumApprovalPrecheckBytes+1))
	if err != nil || len(body) > maximumApprovalPrecheckBytes || !validApprovalPrecheckBFFBody(body) {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_PRECHECK_REQUEST", "invalid approval precheck request", httpserver.RequestID(r.Context()))
		return
	}
	target := *s.config.AccountURL
	target.Path = strings.TrimRight(target.Path, "/") + "/admin/v2/approval-command-prechecks"
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		approvalBFFUnavailable(w, r)
		return
	}
	request.Header.Set("Authorization", "Bearer "+session.AccessToken)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	client := *s.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		approvalBFFUnavailable(w, r)
		return
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maximumApprovalPrecheckBytes+1))
	if err != nil || len(data) > maximumApprovalPrecheckBytes || (response.StatusCode >= 300 && response.StatusCode < 400) {
		approvalBFFUnavailable(w, r)
		return
	}
	// A success answer is the only body the browser may render as a projection;
	// anything unclosed is treated as an unavailable protocol, never as facts.
	if response.StatusCode == http.StatusOK && !validApprovalPrecheckBFFProjection(data) {
		approvalBFFUnavailable(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(data)
}

// validApprovalPrecheckBFFBody accepts only a closed JSON object with unique
// keys and the command/target identities the owner contract requires.
func validApprovalPrecheckBFFBody(raw []byte) bool {
	if len(raw) == 0 || len(raw) > maximumApprovalPrecheckBytes {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return false
	}
	seen := map[string]json.RawMessage{}
	for decoder.More() {
		key, err := decoder.Token()
		name, ok := key.(string)
		if err != nil || !ok || !approvalPrecheckBFFKeys[name] {
			return false
		}
		if _, duplicate := seen[name]; duplicate {
			return false
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return false
		}
		seen[name] = value
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') || decoder.Decode(new(any)) != io.EOF {
		return false
	}
	return closedPrecheckIdentity(seen["command_type"]) && closedPrecheckIdentity(seen["target_resource_id"])
}

func closedPrecheckIdentity(raw json.RawMessage) bool {
	var value string
	return json.Unmarshal(raw, &value) == nil && value != "" && strings.TrimSpace(value) == value
}

// validApprovalPrecheckBFFProjection proves the owner answer is exactly the
// closed projection the page renders: the eight documented fields, positive
// policy/schema versions and three JSON objects.
func validApprovalPrecheckBFFProjection(raw []byte) bool {
	if len(raw) == 0 || len(raw) > maximumApprovalPrecheckBytes {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return false
	}
	seen := map[string]json.RawMessage{}
	for decoder.More() {
		key, err := decoder.Token()
		name, ok := key.(string)
		if err != nil || !ok {
			return false
		}
		if _, duplicate := seen[name]; duplicate {
			return false
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return false
		}
		seen[name] = value
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') || decoder.Decode(new(any)) != io.EOF {
		return false
	}
	for _, required := range []string{"command_type", "policy_version", "snapshot_schema_version", "target_resource_type",
		"target_resource_id", "normalized_command", "critical_resource_versions", "financial_snapshot"} {
		value, ok := seen[required]
		if !ok {
			return false
		}
		switch required {
		case "command_type", "target_resource_type", "target_resource_id":
			if !closedPrecheckIdentity(value) {
				return false
			}
		case "policy_version", "snapshot_schema_version":
			version, err := strconv.ParseInt(string(value), 10, 64)
			if err != nil || version < 1 {
				return false
			}
		default:
			var document map[string]json.RawMessage
			if json.Unmarshal(value, &document) != nil || document == nil {
				return false
			}
		}
	}
	return len(seen) == 8
}
