package portal

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
)

// Directory is a read-only v2 projection for both locked request wires. It
// cannot choose a write protocol or confer permission to decide a request.
func (s *Service) adminApprovalDirectory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_DIRECTORY", "approval directory accepts no query", httpserver.RequestID(r.Context()))
		return
	}
	session, err := s.authenticatedSession(r.Context(), r)
	if err != nil {
		api.WriteError(w, http.StatusUnauthorized, "LOGIN_REQUIRED", "login is required", httpserver.RequestID(r.Context()))
		return
	}
	control, err := s.readApprovalCapability(r.Context(), session)
	if err != nil {
		approvalBFFUnavailable(w, r)
		return
	}
	locker, ok := s.store.(ApprovalWireLocker)
	if !ok {
		approvalBFFUnavailable(w, r)
		return
	}
	wire, err := locker.LockApprovalWireVersion(r.Context(), session, control.ApprovalWireVersion)
	if err != nil || (wire != 1 && wire != 2) || (wire == 2 && control.ApprovalWireVersion != 2) {
		approvalBFFUnavailable(w, r)
		return
	}
	target := *s.config.AccountURL
	target.Path = strings.TrimRight(target.Path, "/") + "/admin/v2/approval-directory"
	target.RawPath, target.RawQuery, target.ForceQuery = "", "", false
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target.String(), nil)
	if err != nil {
		approvalBFFUnavailable(w, r)
		return
	}
	request.Header.Set("Authorization", "Bearer "+session.AccessToken)
	request.Header.Set("Accept", "application/json")
	client := *s.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		approvalBFFUnavailable(w, r)
		return
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusForbidden {
		api.WriteError(w, http.StatusForbidden, "PERMISSION_DENIED", "approval directory access was denied", httpserver.RequestID(r.Context()))
		return
	}
	if response.StatusCode != http.StatusOK {
		approvalBFFUnavailable(w, r)
		return
	}
	media, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	data, readErr := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if mediaErr != nil || media != "application/json" || response.Header.Get("Cache-Control") != "no-store" || readErr != nil || len(data) > 1<<20 {
		approvalBFFUnavailable(w, r)
		return
	}
	var projection struct {
		SchemaVersion                int   `json:"schema_version"`
		PlatformAuthorizationVersion int64 `json:"platform_authorization_version"`
		Items                        []struct {
			CommandType           string `json:"command_type"`
			PolicyVersion         int64  `json:"policy_version"`
			SnapshotSchemaVersion int    `json:"snapshot_schema_version"`
			RiskClass             string `json:"risk_class"`
			Visible               bool   `json:"visible"`
			Requestable           bool   `json:"requestable"`
		} `json:"items"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&projection) != nil || decoder.Decode(new(any)) != io.EOF || projection.SchemaVersion != 1 || projection.PlatformAuthorizationVersion < 1 || projection.Items == nil ||
		response.Header.Get("X-Platform-Authorization-Version") != strconv.FormatInt(projection.PlatformAuthorizationVersion, 10) {
		approvalBFFUnavailable(w, r)
		return
	}
	for _, item := range projection.Items {
		if !currentBillingApprovalCommand(item.CommandType) || item.PolicyVersion < 1 || item.SnapshotSchemaVersion < 1 || item.RiskClass == "" || !item.Visible {
			approvalBFFUnavailable(w, r)
			return
		}
	}
	latest, err := s.readApprovalCapability(r.Context(), session)
	if err != nil || !sameApprovalControl(control, latest) {
		api.WriteError(w, http.StatusConflict, "APPROVAL_ACTION_CONTROL_CHANGED", "approval actions changed; reload the directory", httpserver.RequestID(r.Context()))
		return
	}
	w.Header().Set("X-Platform-Authorization-Version", response.Header.Get("X-Platform-Authorization-Version"))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
