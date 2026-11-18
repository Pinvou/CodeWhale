package account

import (
	"errors"
	"net/http"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/profilerevocation"
)

func (s *Service) claimProfileRevocation(w http.ResponseWriter, r *http.Request) {
	s.profileRevocationReceipt(w, r, false)
}

func (s *Service) confirmProfileRevocation(w http.ResponseWriter, r *http.Request) {
	s.profileRevocationReceipt(w, r, true)
}

func (s *Service) profileRevocationReceipt(w http.ResponseWriter, r *http.Request, confirm bool) {
	var command profilerevocation.Command
	if api.DecodeJSON(w, r, &command) != nil || !validProfileRevocationCommand(command) {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "a profile operation, subject and dispatch token are required", httpserver.RequestID(r.Context()))
		return
	}
	authority, ok := s.profiles.store.(profilerevocation.Authority)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "PROFILE_REVOCATION_UNAVAILABLE", "the receipt authority is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	var result profilerevocation.Result
	var err error
	if confirm {
		err = authority.ConfirmProfileRevocation(r.Context(), command)
	} else {
		result, err = authority.ClaimProfileRevocation(r.Context(), command)
	}
	if errors.Is(err, profilerevocation.ErrConflict) {
		api.WriteError(w, http.StatusConflict, "PROFILE_REVOCATION_CONFLICT", "the profile operation belongs to another identity", httpserver.RequestID(r.Context()))
		return
	}
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "PROFILE_REVOCATION_UNCONFIRMED", "the receipt requires confirmation or operator review", httpserver.RequestID(r.Context()))
		return
	}
	if confirm {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}
