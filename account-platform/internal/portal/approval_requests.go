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
	"github.com/google/uuid"
)

// This is the only version-neutral approval BFF. Its browser path does not
// accept a wire version; the immutable server-side Session lock chooses it.
func (s *Service) adminApprovalRequests(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	resource := strings.Trim(r.PathValue("resource"), "/")
	requestID, detail, decide := approvalBFFTarget(r.Method, resource)
	if !detail && !decide && (resource != "" || r.Method != http.MethodGet) {
		api.WriteError(w, http.StatusNotFound, "NOT_FOUND", "the portal endpoint was not found", httpserver.RequestID(r.Context()))
		return
	}
	if (detail || decide) && r.URL.RawQuery != "" || r.URL.ForceQuery && (detail || decide) {
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
	if decide && !s.validMutation(r, session) {
		api.WriteError(w, http.StatusForbidden, "CSRF_REJECTED", "the request origin or CSRF token is invalid", httpserver.RequestID(r.Context()))
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
	var body []byte
	if decide {
		body, err = io.ReadAll(io.LimitReader(r.Body, 8193))
		if err != nil || len(body) > 8192 || !validApprovalBFFDecision(body, wire, capability.Version, r.Header.Get("X-Pinvou-Approval-Action-Control-Version")) {
			api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_DECISION", "invalid approval decision", httpserver.RequestID(r.Context()))
			return
		}
	}
	target := *s.config.AccountURL
	target.Path = strings.TrimRight(target.Path, "/") + "/admin/v" + strconv.Itoa(wire) + "/approval-requests"
	if detail || decide {
		target.Path += "/" + requestID.String()
	}
	if decide {
		target.Path += "/actions/decide"
	} else if !detail {
		target.RawQuery = r.URL.RawQuery
	}
	request, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		approvalBFFUnavailable(w, r)
		return
	}
	request.Header.Set("Authorization", "Bearer "+session.AccessToken)
	request.Header.Set("Accept", "application/json")
	if decide {
		request.Header.Set("Content-Type", "application/json")
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
	if response.StatusCode == http.StatusOK && detail {
		version := int64(0)
		if wire == 1 {
			version, err = strconv.ParseInt(response.Header.Get("X-Pinvou-Approval-Action-Control-Version"), 10, 64)
		} else {
			var envelope struct {
				ActionControlVersion int64 `json:"action_control_version"`
			}
			err = json.Unmarshal(data, &envelope)
			version = envelope.ActionControlVersion
		}
		if err != nil || version != capability.Version {
			api.WriteError(w, http.StatusConflict, "APPROVAL_ACTION_CONTROL_CHANGED", "approval actions changed; reload the approval detail", httpserver.RequestID(r.Context()))
			return
		}
	}
	if response.StatusCode == http.StatusOK && decide {
		// A decision response is not a fresh detail: v1 has no control-version
		// header. The browser must refetch before offering another action.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
	if detail && wire == 1 && response.StatusCode == http.StatusOK {
		w.Header().Set("X-Pinvou-Approval-Action-Control-Version", response.Header.Get("X-Pinvou-Approval-Action-Control-Version"))
	}
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(data)
}

func approvalBFFTarget(method, resource string) (uuid.UUID, bool, bool) {
	parts := strings.Split(resource, "/")
	if len(parts) == 1 && method == http.MethodGet {
		id, err := uuid.Parse(parts[0])
		return id, err == nil, false
	}
	if len(parts) == 3 && parts[1] == "actions" && parts[2] == "decide" && method == http.MethodPost {
		id, err := uuid.Parse(parts[0])
		return id, false, err == nil
	}
	return uuid.Nil, false, false
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
			!(wire == 1 && name == "observed_versions") && !(wire == 2 && name == "action_control_version") {
			return false
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return false
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
