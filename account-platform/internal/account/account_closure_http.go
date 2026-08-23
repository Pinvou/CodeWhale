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

type accountClosureStore interface {
	CreateAccountClosure(context.Context, identity.Principal, string, string) (AccountClosureRequest, error)
	AccountClosureForPrincipal(context.Context, identity.Principal) (AccountClosureRequest, error)
	ListAccountClosureRepairs(context.Context, AccountClosureRepairFilter) ([]AccountClosureRequest, error)
	RetryAccountClosure(context.Context, identity.Principal, uuid.UUID, string, string) (AccountClosureRequest, error)
}

func (s *Service) closureStore(w http.ResponseWriter, r *http.Request) (accountClosureStore, bool) {
	store, ok := s.repository.(accountClosureStore)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "ACCOUNT_CLOSURE_UNAVAILABLE", "account closure is unavailable", httpserver.RequestID(r.Context()))
	}
	return store, ok
}

func (s *Service) createAccountClosure(w http.ResponseWriter, r *http.Request) {
	store, ok := s.closureStore(w, r)
	if !ok {
		return
	}
	var body struct {
		Confirmation string `json:"confirmation"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	item, err := store.CreateAccountClosure(r.Context(), mustPrincipal(r.Context()), r.Header.Get("Idempotency-Key"), body.Confirmation)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusAccepted, userVisibleClosure(item))
}

func (s *Service) getAccountClosure(w http.ResponseWriter, r *http.Request) {
	store, ok := s.closureStore(w, r)
	if !ok {
		return
	}
	item, err := store.AccountClosureForPrincipal(r.Context(), mustPrincipal(r.Context()))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, userVisibleClosure(item))
}

func (s *Service) adminAccountClosures(w http.ResponseWriter, r *http.Request) {
	store, ok := s.closureStore(w, r)
	if !ok {
		return
	}
	items, err := store.ListAccountClosureRepairs(r.Context(), AccountClosureRepairFilter{Status: r.URL.Query().Get("status"), Stage: r.URL.Query().Get("stage")})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Service) adminRetryAccountClosure(w http.ResponseWriter, r *http.Request) {
	store, ok := s.closureStore(w, r)
	if !ok {
		return
	}
	closureID, parseOK := pathUUID(w, r, "closure_request_id")
	if !parseOK {
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	item, err := store.RetryAccountClosure(r.Context(), mustPrincipal(r.Context()), closureID, key, body.Reason)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, item)
}

func (s *Service) internalAccountClosureState(w http.ResponseWriter, r *http.Request) {
	userID, err := uuid.Parse(r.PathValue("user_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid user identifier", httpserver.RequestID(r.Context()))
		return
	}
	repository, ok := s.repository.(interface {
		AccountClosureState(context.Context, uuid.UUID) (string, int64, error)
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "ACCOUNT_CLOSURE_UNAVAILABLE", "account closure state is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	stage, lifecycleVersion, err := repository.AccountClosureState(r.Context(), userID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"stage": stage, "subject_lifecycle_version": lifecycleVersion})
}
