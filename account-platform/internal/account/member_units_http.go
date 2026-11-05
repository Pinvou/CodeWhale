package account

import (
	"context"
	"net/http"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

type memberUnitsRepository interface {
	ReplaceMemberUnits(context.Context, identity.Principal, uuid.UUID, uuid.UUID, int64, []uuid.UUID, string, bool) (MembershipMutationResult, error)
}

func (s *Service) replaceMemberUnits(w http.ResponseWriter, r *http.Request) {
	s.replaceMemberUnitScope(w, r, false)
}
func (s *Service) adminReplaceMemberUnits(w http.ResponseWriter, r *http.Request) {
	s.replaceMemberUnitScope(w, r, true)
}
func (s *Service) replaceMemberUnitScope(w http.ResponseWriter, r *http.Request, platform bool) {
	repository, ok := s.repository.(memberUnitsRepository)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "ORGANIZATION_UNAVAILABLE", "member attachment service is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	org, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	target, ok := pathUUID(w, r, "user_id")
	if !ok {
		return
	}
	var body struct {
		UnitIDs         []uuid.UUID `json:"unit_ids"`
		ExpectedVersion int64       `json:"expected_version"`
		IdempotencyKey  string      `json:"idempotency_key"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil || body.UnitIDs == nil || body.ExpectedVersion < 1 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "explicit unit_ids and expected_version are required", httpserver.RequestID(r.Context()))
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		key = strings.TrimSpace(body.IdempotencyKey)
	}
	if !validOrganizationUnitIdempotencyKey(key) {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "idempotency key is required", httpserver.RequestID(r.Context()))
		return
	}
	result, err := repository.ReplaceMemberUnits(r.Context(), mustPrincipal(r.Context()), org, target, body.ExpectedVersion, body.UnitIDs, key, platform)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err = s.applyOrganizationMemberLifecycle(r.Context(), result, result.Member.Status); err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "ORGANIZATION_MEMBER_LIFECYCLE_UNAVAILABLE", "member attachment changed locally; retry the same command", httpserver.RequestID(r.Context()))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, result)
}
