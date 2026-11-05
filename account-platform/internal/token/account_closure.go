package token

import (
	"context"
	"net/http"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
)

type AccountClosureBlocker struct {
	Code         string `json:"code"`
	Domain       string `json:"domain"`
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id,omitempty"`
	UserAction   string `json:"user_action,omitempty"`
}

type AccountClosureAssessment struct {
	UserID   uuid.UUID               `json:"user_id"`
	Ready    bool                    `json:"ready"`
	Blockers []AccountClosureBlocker `json:"blockers"`
}

func (r *PostgresRepository) AccountClosureAssessment(ctx context.Context, userID uuid.UUID) (AccountClosureAssessment, error) {
	result := AccountClosureAssessment{UserID: userID, Ready: true, Blockers: []AccountClosureBlocker{}}
	rows, err := r.pool.Query(ctx, `
SELECT request.id,
       CASE
         WHEN request.settlement_status IN ('HOLD_PENDING','HOLD_ACTIVE') THEN 'UNSETTLED_USAGE_HOLD'
         WHEN request.execution_status IN ('DISPATCH_INTENT_PERSISTED','UPSTREAM_DISPATCHING','STREAMING') THEN 'MODEL_EXECUTION_ACTIVE'
         ELSE 'USAGE_RECONCILIATION_PENDING'
       END
FROM usage_requests request
WHERE request.user_id=$1
  AND NOT (request.status IN ('COMMITTED','RELEASED') AND request.settlement_status IN ('SETTLED','RELEASED'))
  AND NOT (request.status='RELEASED' AND request.settlement_status='REVIEW' AND request.cancel_requested_at IS NOT NULL AND EXISTS(SELECT 1 FROM usage_cancel_commands c WHERE c.usage_request_id=request.id AND c.user_id=request.user_id) AND EXISTS(SELECT 1 FROM usage_platform_loss_reviews loss WHERE loss.usage_request_id=request.id AND loss.cause_code='CANCELLATION_RECONCILIATION_DEADLINE' AND loss.customer_debit_prohibited AND loss.provider_cost_evidence->>'customer_hold_released'='true' AND loss.provider_cost_evidence->>'late_usage_customer_debit_prohibited'='true'))
ORDER BY request.updated_at,request.id LIMIT 100`, userID)
	if err != nil {
		return AccountClosureAssessment{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var requestID uuid.UUID
		var code string
		if err := rows.Scan(&requestID, &code); err != nil {
			return AccountClosureAssessment{}, err
		}
		result.Blockers = append(result.Blockers, AccountClosureBlocker{Code: code, Domain: "TOKEN", ResourceType: "USAGE_REQUEST", ResourceID: requestID.String(), UserAction: "WAIT_FOR_USAGE_SETTLEMENT"})
	}
	if err := rows.Err(); err != nil {
		return AccountClosureAssessment{}, err
	}
	result.Ready = len(result.Blockers) == 0
	return result, nil
}

func (s *Service) accountClosureAssessment(w http.ResponseWriter, r *http.Request) {
	userID, err := uuid.Parse(r.PathValue("user_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid user identifier", httpserver.RequestID(r.Context()))
		return
	}
	repository, ok := s.repository.(interface {
		AccountClosureAssessment(context.Context, uuid.UUID) (AccountClosureAssessment, error)
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "ACCOUNT_CLOSURE_UNAVAILABLE", "account closure assessment is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	result, err := repository.AccountClosureAssessment(r.Context(), userID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}
