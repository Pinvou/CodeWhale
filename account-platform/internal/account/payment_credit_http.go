package account

import (
	"context"
	"net/http"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/paymentcredit"
)

func (s *Service) admitPaymentCredit(w http.ResponseWriter, r *http.Request) {
	var input paymentcredit.Request
	if err := api.DecodeJSON(w, r, &input); err != nil || !input.Valid() {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid payment credit admission", httpserver.RequestID(r.Context()))
		return
	}
	repository, ok := s.repository.(interface {
		AdmitPaymentCredit(context.Context, paymentcredit.Request) (paymentcredit.Decision, error)
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "PAYMENT_CREDIT_UNAVAILABLE", "payment credit admission is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	decision, err := repository.AdmitPaymentCredit(r.Context(), input)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, decision)
}

func (s *Service) checkPaymentCreditRetention(w http.ResponseWriter, r *http.Request) {
	var input paymentcredit.Request
	if err := api.DecodeJSON(w, r, &input); err != nil || !input.Valid() {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid payment credit retention check", httpserver.RequestID(r.Context()))
		return
	}
	repository, ok := s.repository.(interface {
		CheckPaymentCreditRetention(context.Context, paymentcredit.Request) (paymentcredit.Decision, error)
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "PAYMENT_CREDIT_UNAVAILABLE", "payment credit retention check is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	decision, err := repository.CheckPaymentCreditRetention(r.Context(), input)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, decision)
}
