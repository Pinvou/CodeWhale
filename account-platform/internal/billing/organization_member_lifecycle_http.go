package billing

import (
	"context"
	"net/http"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
)

func (s *Service) applyOrganizationMemberLifecycle(w http.ResponseWriter, r *http.Request) {
	repository, available := s.repository.(interface {
		ApplyOrganizationMemberLifecycle(context.Context, uuid.UUID, uuid.UUID, string, int64) (OrganizationMemberLifecycleState, error)
	})
	if !available {
		api.WriteError(w, http.StatusServiceUnavailable, "ORGANIZATION_LIFECYCLE_UNAVAILABLE", "organization lifecycle storage is unavailable", httpserver.RequestID(r.Context()))
		return
	}
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
		return
	}
	state, err := repository.ApplyOrganizationMemberLifecycle(r.Context(), organizationID, userID, body.Status, body.MembershipVersion)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, state)
}
