package account

import (
	"context"
	"net/http"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
)

type approvalCapability struct {
	SchemaVersion           int                     `json:"schema_version"`
	Mode                    AdminApprovalMode       `json:"mode"`
	ApprovalWireVersion     int                     `json:"approval_wire_version"`
	ProofRequired           bool                    `json:"proof_required"`
	Epoch                   int64                   `json:"epoch"`
	AuthorizationRelayState AdminApprovalRelayState `json:"authorization_relay_state"`
	AuthorizationRelayEpoch int64                   `json:"authorization_relay_epoch"`
	Version                 int64                   `json:"version"`
	RequestID               string                  `json:"request_id"`
}

// The authenticated client reads one closed control snapshot. This endpoint
// never accepts browser-declared mode, wire version, or proof state.
func (s *Service) adminApprovalCapability(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	reader, ok := s.repository.(interface {
		ApprovalActionControl(context.Context) (AdminApprovalControl, error)
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_UNAVAILABLE", "approval workflow is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	control, err := reader.ApprovalActionControl(r.Context())
	if err != nil || validateAdminApprovalControl(control) != nil || httpserver.RequestID(r.Context()) == "" {
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_UNAVAILABLE", "approval workflow is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	api.WriteJSON(w, http.StatusOK, approvalCapability{
		SchemaVersion: control.SchemaVersion, Mode: control.Mode, ApprovalWireVersion: control.ApprovalWireVersion,
		ProofRequired: control.ProofRequired, Epoch: control.ApprovalEpoch,
		AuthorizationRelayState: control.AuthorizationRelayState, AuthorizationRelayEpoch: control.AuthorizationRelayEpoch,
		Version: control.Version, RequestID: httpserver.RequestID(r.Context()),
	})
}
