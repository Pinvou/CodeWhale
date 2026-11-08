package billing

import (
	"context"
	"net/http"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
)

func (s *Service) organizationUnitDeletionBlockers(w http.ResponseWriter, r *http.Request) {
	repository, available := s.repository.(interface {
		ApplyOrganizationUnitLifecycle(context.Context, uuid.UUID, uuid.UUID, string, int64) (OrganizationUnitDeletionAssessment, error)
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
	unitID, err := uuid.Parse(r.PathValue("unit_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid organization unit identifier", httpserver.RequestID(r.Context()))
		return
	}
	var body struct {
		Status           string `json:"status"`
		StructureVersion int64  `json:"structure_version"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	assessment, err := repository.ApplyOrganizationUnitLifecycle(r.Context(), organizationID, unitID, body.Status, body.StructureVersion)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, assessment)
}
