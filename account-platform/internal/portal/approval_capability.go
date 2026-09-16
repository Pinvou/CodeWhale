package portal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
)

type trustedApprovalCapability struct {
	SchemaVersion           int    `json:"schema_version"`
	Mode                    string `json:"mode"`
	ApprovalWireVersion     int    `json:"approval_wire_version"`
	ProofRequired           bool   `json:"proof_required"`
	Epoch                   int64  `json:"epoch"`
	AuthorizationRelayState string `json:"authorization_relay_state"`
	AuthorizationRelayEpoch int64  `json:"authorization_relay_epoch"`
	Version                 int64  `json:"version"`
	RequestID               string `json:"request_id"`
}

type portalApprovalCapability struct {
	Control            trustedApprovalCapability `json:"control"`
	SessionWireVersion int                       `json:"session_approval_wire_version"`
}

var errApprovalCapabilityUnavailable = errors.New("approval capability unavailable")

func (s *Service) adminApprovalCapability(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_CAPABILITY", "approval capability accepts no query", httpserver.RequestID(r.Context()))
		return
	}
	session, err := s.authenticatedSession(r.Context(), r)
	if err != nil {
		api.WriteError(w, http.StatusUnauthorized, "LOGIN_REQUIRED", "login is required", httpserver.RequestID(r.Context()))
		return
	}
	capability, err := s.readApprovalCapability(r.Context(), session)
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_CAPABILITY_UNAVAILABLE", "approval capability is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	locker, ok := s.store.(ApprovalWireLocker)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_PROTOCOL_UNAVAILABLE", "approval protocol is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	wire, err := locker.LockApprovalWireVersion(r.Context(), session, capability.ApprovalWireVersion)
	if err != nil || (wire != 1 && wire != 2) || (wire == 2 && capability.ApprovalWireVersion != 2) {
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_PROTOCOL_UNAVAILABLE", "approval protocol is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	api.WriteJSON(w, http.StatusOK, portalApprovalCapability{Control: capability, SessionWireVersion: wire})
}

func (s *Service) readApprovalCapability(ctx context.Context, session Session) (trustedApprovalCapability, error) {
	if s == nil || s.config.AccountURL == nil || s.client == nil || session.AccessToken == "" {
		return trustedApprovalCapability{}, errApprovalCapabilityUnavailable
	}
	target := *s.config.AccountURL
	target.Path = strings.TrimRight(target.Path, "/") + "/admin/v2/approval-capability"
	target.RawPath = ""
	target.RawQuery = ""
	target.ForceQuery = false
	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(bounded, http.MethodGet, target.String(), nil)
	if err != nil {
		return trustedApprovalCapability{}, errApprovalCapabilityUnavailable
	}
	request.Header.Set("Authorization", "Bearer "+session.AccessToken)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Cache-Control", "no-cache")
	client := *s.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return trustedApprovalCapability{}, errApprovalCapabilityUnavailable
	}
	defer response.Body.Close()
	media, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode != http.StatusOK || mediaErr != nil || media != "application/json" ||
		len(response.Header.Values("Cache-Control")) != 1 || response.Header.Get("Cache-Control") != "no-store" {
		return trustedApprovalCapability{}, errApprovalCapabilityUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(raw) > 4096 {
		return trustedApprovalCapability{}, errApprovalCapabilityUnavailable
	}
	return decodeTrustedApprovalCapability(raw)
}

func decodeTrustedApprovalCapability(raw []byte) (trustedApprovalCapability, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return trustedApprovalCapability{}, errApprovalCapabilityUnavailable
	}
	fields := map[string]json.RawMessage{}
	expected := map[string]struct{}{"schema_version": {}, "mode": {}, "approval_wire_version": {}, "proof_required": {},
		"epoch": {}, "authorization_relay_state": {}, "authorization_relay_epoch": {}, "version": {}, "request_id": {}}
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		_, allowed := expected[key]
		if err != nil || !ok || !allowed || fields[key] != nil || len(fields) >= 9 {
			return trustedApprovalCapability{}, errApprovalCapabilityUnavailable
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return trustedApprovalCapability{}, errApprovalCapabilityUnavailable
		}
		fields[key] = value
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') || decoder.Decode(new(any)) != io.EOF || len(fields) != 9 {
		return trustedApprovalCapability{}, errApprovalCapabilityUnavailable
	}
	var capability trustedApprovalCapability
	strict := json.NewDecoder(bytes.NewReader(raw))
	strict.DisallowUnknownFields()
	if strict.Decode(&capability) != nil || capability.SchemaVersion != 1 || capability.Epoch < 1 ||
		capability.AuthorizationRelayEpoch < 1 || capability.Version < 1 || capability.RequestID == "" ||
		(capability.AuthorizationRelayState != "RUNNING" && capability.AuthorizationRelayState != "FENCED") {
		return trustedApprovalCapability{}, errApprovalCapabilityUnavailable
	}
	switch capability.Mode {
	case "LEGACY", "QUIESCING":
		if capability.ApprovalWireVersion != 1 || capability.ProofRequired {
			return trustedApprovalCapability{}, errApprovalCapabilityUnavailable
		}
	case "PROOF_REQUIRED":
		if capability.ApprovalWireVersion != 2 || !capability.ProofRequired {
			return trustedApprovalCapability{}, errApprovalCapabilityUnavailable
		}
	case "SAFE_ROLLBACK":
		if capability.ApprovalWireVersion != 1 || !capability.ProofRequired {
			return trustedApprovalCapability{}, errApprovalCapabilityUnavailable
		}
	default:
		return trustedApprovalCapability{}, errApprovalCapabilityUnavailable
	}
	return capability, nil
}
