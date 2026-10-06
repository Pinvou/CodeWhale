package billing

import (
	"net/http"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
)

func (s *Service) applyOrganizationMemberLifecycle(w http.ResponseWriter, r *http.Request) {
	organizationID, err := uuid.Parse(r.PathValue("organization_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid organization identifier", httpserver.RequestID(r.Context()))
		return
	}
	userID, err := uuid.Parse(r.PathValue("user_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid user identifier", httpserver.RequestID(r.Context()))
		return
	}
	var body struct {
		Status            string `json:"status"`
		MembershipVersion int64  `json:"membership_version"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid member lifecycle request", httpserver.RequestID(r.Context()))
		return
	}
	state, err := s.repository.ApplyOrganizationMemberLifecycle(r.Context(), organizationID, userID, body.Status, body.MembershipVersion)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, state)
}
