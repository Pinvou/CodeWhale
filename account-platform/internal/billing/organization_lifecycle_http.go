package billing

import (
	"context"
	"net/http"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
)

func (s *Service) applyOrganizationLifecycle(w http.ResponseWriter, r *http.Request) {
	organizationID, err := uuid.Parse(r.PathValue("organization_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid organization identifier", httpserver.RequestID(r.Context()))
		return
	}
	var body struct {
		Status         string `json:"status"`
		AccountVersion int64  `json:"account_version"`
		PreflightOnly  bool   `json:"preflight_only"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid organization lifecycle request", httpserver.RequestID(r.Context()))
		return
	}
	repository, ok := s.repository.(interface {
		ApplyOrganizationLifecycle(context.Context, uuid.UUID, string, int64, bool) (OrganizationLifecycleAssessment, error)
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "ORGANIZATION_LIFECYCLE_UNAVAILABLE", "organization lifecycle is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	result, err := repository.ApplyOrganizationLifecycle(r.Context(), organizationID, strings.ToUpper(strings.TrimSpace(body.Status)), body.AccountVersion, body.PreflightOnly)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	status := http.StatusOK
	if !result.Ready {
		status = http.StatusConflict
	}
	api.WriteJSON(w, status, result)
}
