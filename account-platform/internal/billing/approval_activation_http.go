package billing

import (
	"net/http"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
)

func (s *Service) approvalCommandActivations(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	repository, ok := s.repository.(approvalActivationRepository)
	if !ok {
		s.writeError(w, r, ErrApprovalActivationUnavailable)
		return
	}
	snapshot, err := repository.ApprovalCommandActivations(r.Context())
	if err != nil || validateApprovalCommandActivationSnapshot(snapshot, s.now()) != nil {
		s.writeError(w, r, ErrApprovalActivationUnavailable)
		return
	}
	api.WriteJSON(w, http.StatusOK, snapshot)
}
