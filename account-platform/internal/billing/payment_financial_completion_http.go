package billing

import (
	"net/http"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
)

func (s *Service) adminProposeFinancialCompletion(w http.ResponseWriter, r *http.Request) {
	s.adminFinancialCompletion(w, r, false)
}

func (s *Service) adminConfirmFinancialCompletion(w http.ResponseWriter, r *http.Request) {
	s.adminFinancialCompletion(w, r, true)
}

func (s *Service) adminFinancialCompletion(w http.ResponseWriter, r *http.Request, confirm bool) {
	caseID, err := uuid.Parse(r.PathValue("case_id"))
	proposalID := uuid.Nil
	if confirm && err == nil {
		proposalID, err = uuid.Parse(r.PathValue("proposal_id"))
		if proposalID == uuid.Nil {
			err = ErrConflict
		}
	}
	var body struct {
		ProofKind   string    `json:"proof_kind"`
		ReferenceID uuid.UUID `json:"reference_id"`
		Reason      string    `json:"reason"`
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if err != nil || caseID == uuid.Nil || api.DecodeJSON(w, r, &body) != nil || len(key) < 8 || len(key) > 120 || len([]rune(strings.TrimSpace(body.Reason))) < 10 || len([]rune(strings.TrimSpace(body.Reason))) > 2000 || (confirm && (body.ProofKind != "" || body.ReferenceID != uuid.Nil)) {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "valid case, completion facts, reason and idempotency key are required", httpserver.RequestID(r.Context()))
		return
	}
	if s.payments == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "PAYMENT_COMPLETION_UNAVAILABLE", "payment financial completion is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	result, err := s.payments.PaymentFinancialCompletion(r.Context(), caseID, adminActor(adminPrincipal(r)), key, body.Reason, PaymentFinancialCompletionInput{ProofKind: body.ProofKind, ReferenceID: body.ReferenceID}, proposalID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeAdminFinancialJSON(w, http.StatusOK, result)
}
