package billing

import (
	"context"
	"net/http"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/paymentcredit"
)

func (s *Service) resolvePaymentCredit(w http.ResponseWriter, r *http.Request) {
	var input paymentcredit.Request
	if err := api.DecodeJSON(w, r, &input); err != nil || !input.Valid() {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid payment credit outcome", httpserver.RequestID(r.Context()))
		return
	}
	repository, ok := s.repository.(interface {
		ResolvePaymentCredit(context.Context, paymentcredit.Request) (paymentcredit.Decision, error)
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "PAYMENT_CREDIT_UNAVAILABLE", "payment credit outcome is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	decision, err := repository.ResolvePaymentCredit(r.Context(), input)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, decision)
}

func (s *Service) acknowledgePaymentCredit(w http.ResponseWriter, r *http.Request) {
	var input paymentcredit.Acknowledgement
	if err := api.DecodeJSON(w, r, &input); err != nil || !input.Request.Valid() || (input.State != "APPLIED" && input.State != "ABORTED") {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid payment credit acknowledgement", httpserver.RequestID(r.Context()))
		return
	}
	repository, ok := s.repository.(interface {
		AcknowledgePaymentCredit(context.Context, paymentcredit.Request, string) (paymentcredit.Decision, error)
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "PAYMENT_CREDIT_UNAVAILABLE", "payment credit acknowledgement is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	decision, err := repository.AcknowledgePaymentCredit(r.Context(), input.Request, input.State)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, decision)
}
