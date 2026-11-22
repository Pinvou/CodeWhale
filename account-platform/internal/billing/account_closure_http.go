package billing

import (
	"context"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
	"net/http"
)

func (s *Service) isolateAccountClosedGrants(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.repository.(interface {
		IsolateAccountClosedGrants(context.Context, uuid.UUID, uuid.UUID, int64) (AccountClosureGrantIsolation, error)
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "ACCOUNT_CLOSURE_UNAVAILABLE", "account closure isolation is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	userID, err := uuid.Parse(r.PathValue("user_id"))
	var body struct {
		ClosureRequestID        uuid.UUID `json:"closure_request_id"`
		SubjectLifecycleVersion int64     `json:"subject_lifecycle_version"`
	}
	if err != nil || api.DecodeJSON(w, r, &body) != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid account closure grant isolation", httpserver.RequestID(r.Context()))
		return
	}
	result, err := repository.IsolateAccountClosedGrants(r.Context(), body.ClosureRequestID, userID, body.SubjectLifecycleVersion)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}
