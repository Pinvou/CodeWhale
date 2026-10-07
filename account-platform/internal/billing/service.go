package billing

import (
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"

	"net/http"

	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"

	"github.com/google/uuid"
)

func (s *Service) ensureWallet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OwnerType string    `json:"owner_type"`
		OwnerID   uuid.UUID `json:"owner_id"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil || body.OwnerID == uuid.Nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid wallet owner", httpserver.RequestID(r.Context()))
		return
	}
	wallet, created, err := s.repository.EnsureWallet(r.Context(), strings.ToUpper(body.OwnerType), body.OwnerID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	api.WriteJSON(w, status, wallet)
}

func (s *Service) getBalance(w http.ResponseWriter, r *http.Request) {
	walletID, err := uuid.Parse(r.PathValue("wallet_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid wallet identifier", httpserver.RequestID(r.Context()))
		return
	}
	wallet, err := s.repository.GetWallet(r.Context(), walletID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, wallet)
}

type Service struct{ repository Repository }

func NewService(repository Repository) *Service { return &Service{repository: repository} }

func (s *Service) Handler(serviceToken string) http.Handler {
	router := httpserver.NewRouter()
	protected := router.Group("")
	protected.Use(serviceauth.GinMiddleware(serviceToken))
	protected.POST("/internal/v1/wallets/ensure", serviceauth.Require("internal.billing.wallet.ensure"), httpserver.AdaptFunc(s.ensureWallet))
	protected.GET("/internal/v1/wallets/:wallet_id/balance", serviceauth.Require("internal.billing.wallet.read"), httpserver.AdaptFunc(s.getBalance))
	return router
}

func (s *Service) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := http.StatusInternalServerError, "INTERNAL_ERROR", "the billing request could not be completed"
	switch {
	case errors.Is(err, ErrNotFound):
		status, code, message = http.StatusNotFound, "NOT_FOUND", "the billing resource was not found"
	case errors.Is(err, ErrConflict):
		status, code, message = http.StatusConflict, "BILLING_CONFLICT", "the billing request conflicts with existing state"
	}
	api.WriteError(w, status, code, message, httpserver.RequestID(r.Context()))
}
