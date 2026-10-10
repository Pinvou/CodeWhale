package billing

import (
	"context"
	"errors"
	"net/http"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
	var walletID uuid.UUID
	var posted, receivable int64
	err := r.pool.QueryRow(ctx, `SELECT id,posted_microcredits,receivable_microcredits FROM wallets WHERE owner_type='PERSONAL' AND owner_id=$1`, userID).Scan(&walletID, &posted, &receivable)
	if errors.Is(err, pgx.ErrNoRows) {
		result.Blockers = append(result.Blockers, AccountClosureBlocker{Code: "PERSONAL_WALLET_UNAVAILABLE", Domain: "BILLING", ResourceType: "WALLET", UserAction: "CONTACT_SUPPORT"})
	} else if err != nil {
		return AccountClosureAssessment{}, err
	} else {
		if posted != 0 {
			result.Blockers = append(result.Blockers, AccountClosureBlocker{Code: "PERSONAL_BALANCE_NONZERO", Domain: "BILLING", ResourceType: "WALLET", ResourceID: walletID.String(), UserAction: "CLEAR_PERSONAL_BALANCE"})
		}
		if receivable != 0 {
			result.Blockers = append(result.Blockers, AccountClosureBlocker{Code: "RECEIVABLE_OUTSTANDING", Domain: "BILLING", ResourceType: "RECEIVABLE", ResourceID: walletID.String(), UserAction: "SETTLE_RECEIVABLE"})
		}
	}
	checks := []struct {
		code, resourceType, action, query string
	}{
		{"UNSETTLED_HOLD", "USAGE_HOLD", "WAIT_FOR_USAGE_SETTLEMENT", `SELECT EXISTS(SELECT 1 FROM usage_holds hold JOIN wallets wallet ON wallet.id=hold.wallet_id WHERE wallet.owner_type='PERSONAL' AND wallet.owner_id=$1 AND hold.status IN ('ACTIVE','REVIEW'))`},
		{"PAYMENT_PROCESSING", "PAYMENT_ORDER", "FINISH_PAYMENT_ORDER", `SELECT EXISTS(SELECT 1 FROM payment_orders WHERE payer_user_id=$1 AND (status IN ('CREATED','PENDING','MANUAL_REVIEW') OR close_status IN ('CLOSE_REQUESTED','LOCALLY_CLOSED_EXTERNAL_PENDING') OR attention_summary IN ('PROCESSING','REVIEW_REQUIRED','RECEIVABLE_OPEN')))`},
		{"REFUND_UNRESOLVED", "REFUND", "FINISH_REFUND", `SELECT EXISTS(SELECT 1 FROM refunds refund JOIN payment_orders payment ON payment.id=refund.payment_order_id WHERE payment.payer_user_id=$1 AND refund.status IN ('REQUESTED','MANUAL_REVIEW'))`},
		{"CHARGEBACK_UNRESOLVED", "CHARGEBACK", "RESOLVE_CHARGEBACK", `SELECT EXISTS(SELECT 1 FROM chargeback_cases source JOIN payment_orders payment ON payment.id=source.payment_order_id WHERE payment.payer_user_id=$1 AND NOT COALESCE(
(source.status='CONFIRMED' AND EXISTS(SELECT 1 FROM journal_entries WHERE entry_type='CHARGEBACK' AND business_reference='payment-transaction:'||source.payment_transaction_id::text||':chargeback:'||source.provider_chargeback_id))
OR (source.status='DISMISSED' AND source.resolution_code='DISMISS' AND EXISTS(SELECT 1 FROM payment_anomaly_cases anomaly JOIN payment_anomaly_commands command ON command.payment_anomaly_case_id=anomaly.id AND command.action='DISMISS' WHERE anomaly.id=source.manual_review_case_id AND anomaly.chargeback_case_id=source.id AND anomaly.status='RESOLVED' AND anomaly.resolution_code='DISMISS' AND anomaly.financial_action_status='NONE')),false))`},
		{"FORCED_REFUND_UNRESOLVED", "FORCED_REFUND", "FINISH_REFUND", `SELECT EXISTS(SELECT 1 FROM forced_refund_cases source JOIN payment_orders payment ON payment.id=source.payment_order_id WHERE payment.payer_user_id=$1 AND NOT COALESCE(
(source.status='RESOLVED' AND source.resolution_code='CONFIRM_FORCED_REFUND' AND source.version>1 AND EXISTS(SELECT 1 FROM journal_entries WHERE entry_type='FORCED_REFUND' AND business_reference='forced-refund:'||source.id::text))
OR (source.status='RESOLVED' AND source.resolution_code='DISMISS' AND EXISTS(SELECT 1 FROM payment_anomaly_cases anomaly JOIN payment_anomaly_commands command ON command.payment_anomaly_case_id=anomaly.id AND command.action='DISMISS' WHERE anomaly.id=source.manual_review_case_id AND anomaly.forced_refund_case_id=source.id AND anomaly.status='RESOLVED' AND anomaly.resolution_code='DISMISS' AND anomaly.financial_action_status='NONE')),false))`},
		{"FINANCIAL_RECONCILIATION_OPEN", "RECONCILIATION_CASE", "RESOLVE_FINANCIAL_RECONCILIATION", `SELECT EXISTS(SELECT 1 FROM reconciliation_cases reconciliation JOIN payment_orders payment ON payment.id=reconciliation.payment_order_id WHERE payment.payer_user_id=$1 AND reconciliation.status IN ('OPEN','IN_REVIEW') AND reconciliation.case_type<>'UNSUPPORTED_PROVIDER_ENTRY')`},
		{"LATE_PAYMENT_REVIEW_OPEN", "LATE_PAYMENT_REVIEW", "FINISH_LATE_PAYMENT_REVIEW", `SELECT EXISTS(SELECT 1 FROM late_payment_review_cases review JOIN payment_orders payment ON payment.id=review.payment_order_id WHERE payment.payer_user_id=$1 AND review.review_status IN ('REVIEW_REQUIRED_LATE_PAYMENT','PROVIDER_BLIND'))`},
		{"PAYMENT_ANOMALY_OPEN", "PAYMENT_ANOMALY", "RESOLVE_PAYMENT_ANOMALY", `SELECT EXISTS(SELECT 1 FROM payment_anomaly_cases anomaly JOIN payment_orders payment ON payment.id=anomaly.payment_order_id WHERE payment.payer_user_id=$1 AND anomaly.status='OPEN')`},
	}
	for _, check := range checks {
		var exists bool
		if err := r.pool.QueryRow(ctx, check.query, userID).Scan(&exists); err != nil {
			return AccountClosureAssessment{}, err
		}
		if exists {
			result.Blockers = append(result.Blockers, AccountClosureBlocker{Code: check.code, Domain: "BILLING", ResourceType: check.resourceType, ResourceID: userID.String(), UserAction: check.action})
		}
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
