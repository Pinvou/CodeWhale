package portal

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
)

// This is the only version-neutral approval BFF. Its browser path does not
// accept a wire version; the immutable server-side Session lock chooses it.
func (s *Service) adminApprovalRequests(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	resource := strings.Trim(r.PathValue("resource"), "/")
	requestID, kind := approvalBFFTarget(r.Method, resource)
	if kind == "" {
		api.WriteError(w, http.StatusNotFound, "NOT_FOUND", "the portal endpoint was not found", httpserver.RequestID(r.Context()))
		return
	}
	mutation := kind != "list" && kind != "detail" && kind != "withdraw-result" && kind != "decision-result"
	if kind != "list" && kind != "withdraw-result" && kind != "decision-result" && (r.URL.RawQuery != "" || r.URL.ForceQuery) {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_REQUEST", "approval detail accepts no query", httpserver.RequestID(r.Context()))
		return
	}
	for key := range r.Header {
		if strings.HasPrefix(strings.ToLower(key), "x-pinvou-approval-proof-") {
			api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_DECISION", "browser proof is not accepted", httpserver.RequestID(r.Context()))
			return
		}
	}
	session, err := s.authenticatedSession(r.Context(), r)
	if err != nil {
		api.WriteError(w, http.StatusUnauthorized, "LOGIN_REQUIRED", "login is required", httpserver.RequestID(r.Context()))
		return
	}
	if mutation && !s.validMutation(r, session) {
		api.WriteError(w, http.StatusForbidden, "CSRF_REJECTED", "the request origin or CSRF token is invalid", httpserver.RequestID(r.Context()))
		return
	}
	if mutation && kind != "create" && !s.config.AdminModelTokenActionsEnabled && s.rejectDisabledExistingTokenApprovalAction(w, r, requestID) {
		return
	}
	capability, err := s.readApprovalCapability(r.Context(), session)
	if err != nil {
		approvalBFFUnavailable(w, r)
		return
	}
	locker, ok := s.store.(ApprovalWireLocker)
	if !ok {
		approvalBFFUnavailable(w, r)
		return
	}
	wire, err := locker.LockApprovalWireVersion(r.Context(), session, capability.ApprovalWireVersion)
	if err != nil || (wire != 1 && wire != 2) || (wire == 2 && capability.ApprovalWireVersion != 2) {
		approvalBFFUnavailable(w, r)
		return
	}
	if kind == "withdraw-result" && !validApprovalWithdrawalResultQuery(r, capability.Version) {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_REQUEST", "invalid withdrawal confirmation", httpserver.RequestID(r.Context()))
		return
	}
	if kind == "decision-result" && !validApprovalDecisionResultQuery(r, capability.Version) {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_REQUEST", "invalid decision confirmation", httpserver.RequestID(r.Context()))
		return
	}
	var body []byte
	if mutation {
		limit := int64(8192)
		if kind == "create" || kind == "update" {
			limit = 4 << 20
		}
		body, err = io.ReadAll(io.LimitReader(r.Body, limit+1))
		if err != nil || int64(len(body)) > limit || !validApprovalBFFMutation(kind, body, wire, capability.Version, r.Header.Get("X-Pinvou-Approval-Action-Control-Version")) {
			api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_REQUEST", "invalid approval mutation", httpserver.RequestID(r.Context()))
			return
		}
	}
	if kind == "create" && !s.config.AdminModelTokenActionsEnabled && rejectDisabledBFFTokenDraft(w, r, body) {
		return
	}
	target := *s.config.AccountURL
	target.Path = strings.TrimRight(target.Path, "/") + "/admin/v" + strconv.Itoa(wire) + "/approval-requests"
	if kind != "list" && kind != "create" {
		target.Path += "/" + requestID.String()
	}
	if kind == "decide" || kind == "submit" || kind == "withdraw" || kind == "withdraw-result" || kind == "decision-result" {
		target.Path += "/actions/" + kind
	} else if kind == "list" {
		target.RawQuery = r.URL.RawQuery
	}
	if kind == "withdraw-result" || kind == "decision-result" {
		target.RawQuery = r.URL.RawQuery
	}
	request, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		approvalBFFUnavailable(w, r)
		return
	}
	request.Header.Set("Authorization", "Bearer "+session.AccessToken)
	request.Header.Set("Accept", "application/json")
	if mutation {
		request.Header.Set("Content-Type", "application/json")
	}
	if wire == 2 && mutation && kind != "decide" || kind == "withdraw" || kind == "withdraw-result" || kind == "decide" || kind == "decision-result" {
		request.Header.Set("X-Pinvou-Approval-Action-Control-Version", strconv.FormatInt(capability.Version, 10))
	}
	client := *s.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		approvalBFFUnavailable(w, r)
		return
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 4<<20+1))
	if err != nil || len(data) > 4<<20 || response.StatusCode >= 300 && response.StatusCode < 400 {
		approvalBFFUnavailable(w, r)
		return
	}
	latest, err := s.readApprovalCapability(r.Context(), session)
	if err != nil || !sameApprovalControl(capability, latest) {
		api.WriteError(w, http.StatusConflict, "APPROVAL_ACTION_CONTROL_CHANGED", "approval actions changed; reload the approval detail", httpserver.RequestID(r.Context()))
		return
	}
	if response.StatusCode == http.StatusOK && kind == "detail" {
		version := int64(0)
		if wire == 1 {
			version, err = strconv.ParseInt(response.Header.Get("X-Pinvou-Approval-Action-Control-Version"), 10, 64)
		} else {
			var envelope struct {
				ActionControlVersion int64           `json:"action_control_version"`
				DetailAvailability   string          `json:"detail_availability"`
				AllowedActions       json.RawMessage `json:"allowed_actions"`
			}
			err = json.Unmarshal(data, &envelope)
			version = envelope.ActionControlVersion
			if err == nil && version == 0 && envelope.DetailAvailability == "DETAIL_PRESENTATION_UNAVAILABLE" && len(envelope.AllowedActions) == 0 {
				version = capability.Version // Historical minimum has no action-bearing generation.
			}
		}
		if err != nil || version != capability.Version {
			api.WriteError(w, http.StatusConflict, "APPROVAL_ACTION_CONTROL_CHANGED", "approval actions changed; reload the approval detail", httpserver.RequestID(r.Context()))
			return
		}
	}
	if kind == "withdraw-result" && response.StatusCode == http.StatusOK && !validApprovalWithdrawalResult(data, requestID, r, capability.Version) {
		approvalBFFUnavailable(w, r)
		return
	}
	if kind == "decision-result" && response.StatusCode == http.StatusOK && !validApprovalDecisionResult(data, requestID, r, capability.Version) {
		approvalBFFUnavailable(w, r)
		return
	}
	if kind == "detail" && response.StatusCode == http.StatusOK {
		response.Body = io.NopCloser(bytes.NewReader(data))
		s.captureApprovalClassification(r.Context(), session, r.Method, "/admin/v"+strconv.Itoa(wire)+"/approval-requests/"+requestID.String(), response)
	}
	if mutation && response.StatusCode >= 200 && response.StatusCode < 300 && kind != "create" {
		if response.StatusCode != http.StatusNoContent {
			id, valid := approvalCreatedID(data, wire)
			if response.StatusCode != http.StatusOK || !valid || id != requestID {
				approvalBFFUnavailable(w, r)
				return
			}
		}
		// A mutation response is not a fresh detail. The browser must refetch.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if kind == "create" && response.StatusCode == http.StatusCreated {
		id, ok := approvalCreatedID(data, wire)
		if !ok {
			approvalBFFUnavailable(w, r)
			return
		}
		api.WriteJSON(w, http.StatusCreated, map[string]string{"approval_request_id": id.String()})
		return
	}
	w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
	if kind == "detail" && wire == 1 && response.StatusCode == http.StatusOK {
		w.Header().Set("X-Pinvou-Approval-Action-Control-Version", response.Header.Get("X-Pinvou-Approval-Action-Control-Version"))
	}
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(data)
}

func approvalBFFTarget(method, resource string) (uuid.UUID, string) {
	if resource == "" {
		if method == http.MethodGet {
			return uuid.Nil, "list"
		}
		if method == http.MethodPost {
			return uuid.Nil, "create"
		}
		return uuid.Nil, ""
	}
	parts := strings.Split(resource, "/")
	if len(parts) == 1 && (method == http.MethodGet || method == http.MethodPut) {
		id, err := uuid.Parse(parts[0])
		if err != nil || id == uuid.Nil || id.String() != parts[0] {
			return uuid.Nil, ""
		}
		if method == http.MethodGet {
			return id, "detail"
		}
		return id, "update"
	}
	if len(parts) == 3 && parts[1] == "actions" && (parts[2] == "withdraw-result" || parts[2] == "decision-result") && method == http.MethodGet {
		id, err := uuid.Parse(parts[0])
		if err == nil && id != uuid.Nil && id.String() == parts[0] {
			return id, parts[2]
		}
	}
	if len(parts) == 3 && parts[1] == "actions" && method == http.MethodPost && (parts[2] == "decide" || parts[2] == "submit" || parts[2] == "withdraw") {
		id, err := uuid.Parse(parts[0])
		if err == nil && id != uuid.Nil && id.String() == parts[0] {
			return id, parts[2]
		}
	}
	return uuid.Nil, ""
}

func validApprovalBFFMutation(kind string, raw []byte, wire int, version int64, header string) bool {
	observed, err := strconv.ParseInt(header, 10, 64)
	if err != nil || observed != version {
		return false
	}
	if kind == "decide" {
		return len(raw) <= 8192 && validApprovalBFFDecision(raw, wire, version, header)
	}
	allowed := map[string]bool{"expected_version": true}
	if kind == "withdraw" {
		allowed["command_protocol"] = true
	}
	if kind == "create" || kind == "update" {
		allowed = map[string]bool{
			"command_type": true, "idempotency_key": true, "normalized_command": true,
			"target_resource_type": true, "target_resource_id": true, "reason": true,
			"redacted_evidence": true, "expected_impact": true,
			"critical_resource_versions": true, "financial_snapshot": true,
			"supersedes_request_id": true,
		}
		if kind == "update" {
			allowed["expected_version"] = true
		}
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
		if err != nil || !ok || !allowed[name] {
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
	if protocol, present := seen["command_protocol"]; present {
		var version int
		if kind != "withdraw" || json.Unmarshal(protocol, &version) != nil || version != 1 {
			return false
		}
	}
	if kind == "create" || kind == "update" {
		var commandType string
		if json.Unmarshal(seen["command_type"], &commandType) != nil {
			return false
		}
		if commandType == "ENTITLEMENT_COMPONENT_ADJUSTMENT" {
			// This command was delivered only as policy2/snapshot2. Preserve the
			// legacy six-command contract and validate the exact owner facts.
			var targetType, targetID string
			if wire != 2 || json.Unmarshal(seen["target_resource_type"], &targetType) != nil || json.Unmarshal(seen["target_resource_id"], &targetID) != nil {
				return false
			}
			if _, err := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{CommandType: commandType, PolicyVersion: 2, SnapshotSchemaVersion: 2, TargetResourceType: targetType, TargetResourceID: targetID, NormalizedCommand: seen["normalized_command"], CriticalResourceVersions: seen["critical_resource_versions"], FinancialSnapshot: seen["financial_snapshot"]}); err != nil {
				return false
			}
		} else if !currentGovernedApprovalCommand(commandType) {
			return false
		}
	}
	if kind != "create" {
		expected, err := strconv.ParseInt(string(seen["expected_version"]), 10, 64)
		if err != nil || expected < 1 {
			return false
		}
	}
	return true
}

func approvalCreatedID(raw []byte, wire int) (uuid.UUID, bool) {
	var envelope struct {
		ApprovalRequestID uuid.UUID `json:"approval_request_id"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return uuid.Nil, false
	}
	if (wire == 1 || wire == 2) && envelope.ApprovalRequestID != uuid.Nil {
		return envelope.ApprovalRequestID, true
	}
	return uuid.Nil, false
}

func validApprovalBFFDecision(raw []byte, wire int, version int64, header string) bool {
	observed, err := strconv.ParseInt(header, 10, 64)
	if err != nil || observed != version {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	for decoder.More() {
		key, err := decoder.Token()
		name, ok := key.(string)
		if err != nil || !ok || seen[name] {
			return false
		}
		seen[name] = true
		if name != "expected_version" && name != "decision" && name != "reason" &&
			!(wire == 1 && name == "observed_versions") && !(wire == 2 && name == "action_control_version") && name != "command_protocol" {
			return false
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return false
		}
		if name == "command_protocol" {
			var protocol int
			if json.Unmarshal(value, &protocol) != nil || protocol != 1 {
				return false
			}
		}
		if name == "action_control_version" {
			parsed, err := strconv.ParseInt(string(value), 10, 64)
			if err != nil || parsed != version {
				return false
			}
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') || decoder.Decode(new(any)) != io.EOF {
		return false
	}
	return seen["expected_version"] && seen["decision"] && seen["reason"] && (wire == 1 || seen["action_control_version"])
}

func sameApprovalControl(a, b trustedApprovalCapability) bool {
	a.RequestID, b.RequestID = "", ""
	return a == b
}

func approvalBFFUnavailable(w http.ResponseWriter, r *http.Request) {
	api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_PROTOCOL_UNAVAILABLE", "approval protocol is unavailable", httpserver.RequestID(r.Context()))
}

// This endpoint cannot mutate an approval or upgrade its frozen write control.
func validApprovalWithdrawalResultQuery(r *http.Request, control int64) bool {
	q := r.URL.Query()
	if len(q) != 3 || len(q["command_protocol"]) != 1 || q.Get("command_protocol") != "1" {
		return false
	}
	for _, name := range []string{"expected_version", "origin_action_control_version"} {
		values := q[name]
		if len(values) != 1 {
			return false
		}
		v, e := strconv.ParseInt(values[0], 10, 64)
		if e != nil || v < 1 || values[0] != strconv.FormatInt(v, 10) {
			return false
		}
	}
	values := r.Header.Values("X-Pinvou-Approval-Action-Control-Version")
	return len(values) == 1 && values[0] == strconv.FormatInt(control, 10)
}

func validApprovalWithdrawalResult(raw []byte, id uuid.UUID, r *http.Request, control int64) bool {
	var receipt struct {
		ID        uuid.UUID `json:"approval_request_id"`
		Protocol  int       `json:"command_protocol"`
		Operation string    `json:"operation"`
		Expected  int64     `json:"expected_version"`
		Result    int64     `json:"result_version"`
		Origin    int64     `json:"origin_action_control_version"`
		Control   int64     `json:"action_control_version"`
		Event     uuid.UUID `json:"receipt_event_id"`
		Status    string    `json:"status"`
		At        time.Time `json:"recorded_at"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&receipt) != nil || d.Decode(new(any)) != io.EOF {
		return false
	}
	return receipt.ID == id && receipt.Protocol == 1 && receipt.Operation == "WITHDRAW" && receipt.Status == "WITHDRAWN" &&
		receipt.Expected > 0 && receipt.Expected < receipt.Result && receipt.Result-receipt.Expected == 1 &&
		strconv.FormatInt(receipt.Expected, 10) == r.URL.Query().Get("expected_version") &&
		strconv.FormatInt(receipt.Origin, 10) == r.URL.Query().Get("origin_action_control_version") &&
		receipt.Control == control && receipt.Event != uuid.Nil && !receipt.At.IsZero()
}

func validApprovalDecisionDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func validApprovalDecisionResultQuery(r *http.Request, control int64) bool {
	q := r.URL.Query()
	if len(q) != 5 || len(q["command_protocol"]) != 1 || q.Get("command_protocol") != "1" {
		return false
	}
	for _, key := range []string{"expected_version", "origin_action_control_version"} {
		v := q[key]
		if len(v) != 1 {
			return false
		}
		n, e := strconv.ParseInt(v[0], 10, 64)
		if e != nil || n < 1 || strconv.FormatInt(n, 10) != v[0] {
			return false
		}
	}
	if len(q["decision"]) != 1 || (q.Get("decision") != "APPROVED" && q.Get("decision") != "REJECTED") || len(q["reason_digest"]) != 1 || !validApprovalDecisionDigest(q.Get("reason_digest")) {
		return false
	}
	values := r.Header.Values("X-Pinvou-Approval-Action-Control-Version")
	return len(values) == 1 && values[0] == strconv.FormatInt(control, 10)
}
func validApprovalDecisionResult(raw []byte, id uuid.UUID, r *http.Request, control int64) bool {
	var receipt struct {
		ID         uuid.UUID `json:"approval_request_id"`
		Protocol   int       `json:"command_protocol"`
		Operation  string    `json:"operation"`
		Decision   string    `json:"decision"`
		Expected   int64     `json:"expected_version"`
		Result     int64     `json:"result_version"`
		Origin     int64     `json:"origin_action_control_version"`
		Control    int64     `json:"action_control_version"`
		Digest     string    `json:"reason_digest"`
		Event      uuid.UUID `json:"receipt_event_id"`
		DecisionID uuid.UUID `json:"decision_id"`
		At         time.Time `json:"recorded_at"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&receipt) != nil || d.Decode(new(any)) != io.EOF {
		return false
	}
	q := r.URL.Query()
	return receipt.ID == id && receipt.Protocol == 1 && receipt.Operation == "DECIDE" && receipt.Decision == q.Get("decision") && receipt.Expected > 0 && receipt.Result > receipt.Expected && receipt.Result-receipt.Expected == 1 && strconv.FormatInt(receipt.Expected, 10) == q.Get("expected_version") && strconv.FormatInt(receipt.Origin, 10) == q.Get("origin_action_control_version") && receipt.Control == control && receipt.Digest == q.Get("reason_digest") && validApprovalDecisionDigest(receipt.Digest) && receipt.Event != uuid.Nil && receipt.DecisionID != uuid.Nil && !receipt.At.IsZero()
}
