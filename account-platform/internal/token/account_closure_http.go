package token

import (
	"context"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
	"net/http"
)

func (s *Service) anonymizeUserReplayPayloads(w http.ResponseWriter, r *http.Request) {
	userID, err := uuid.Parse(r.PathValue("user_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid user", httpserver.RequestID(r.Context()))
		return
	}
	repository, ok := s.repository.(interface {
		AnonymizeUsageReplay(context.Context, uuid.UUID) error
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "REPLAY_RETENTION_UNAVAILABLE", "replay retention is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	if err := repository.AnonymizeUsageReplay(r.Context(), userID); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
