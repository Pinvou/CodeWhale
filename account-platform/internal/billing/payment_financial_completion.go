package billing

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const paymentCompletionPropose = "PROPOSE_ESCALATED_FINANCIAL_COMPLETION"
const paymentCompletionConfirm = "CONFIRM_ESCALATED_FINANCIAL_COMPLETION"

type PaymentFinancialCompletionProposal struct {
	ID               uuid.UUID                       `json:"proposal_id"`
	CaseID           uuid.UUID                       `json:"case_id"`
	CaseVersion      int64                           `json:"case_version"`
	ProposerID       uuid.UUID                       `json:"proposer_id"`
	Input            PaymentFinancialCompletionInput `json:"input"`
	Facts            PaymentFinancialCompletionFacts `json:"facts"`
	Reason           string                          `json:"reason"`
	CreatedAt        time.Time                       `json:"created_at"`
	QueryStartedAt   *time.Time                      `json:"query_started_at,omitempty"`
	ProviderSnapshot json.RawMessage                 `json:"provider_snapshot,omitempty"`
}

type PaymentFinancialCompletionResult struct {
	CaseID   uuid.UUID                          `json:"case_id"`
	Case     PaymentAnomalyCase                 `json:"case"`
	Proposal PaymentFinancialCompletionProposal `json:"proposal"`
}

type paymentCompletionRequest struct {
	Action     string
	Input      PaymentFinancialCompletionInput
	ProposalID uuid.UUID
	Reason     string
}

func loadPaymentCompletionReplay(ctx context.Context, source paymentRepairReader, caseID, actorID uuid.UUID, key string, request paymentCompletionRequest) (PaymentFinancialCompletionResult, bool, error) {
	var actor uuid.UUID
	var hash, action string
	var snapshot json.RawMessage
	err := source.QueryRow(ctx, `SELECT actor_id,request_hash,action,after_snapshot FROM payment_anomaly_commands WHERE payment_anomaly_case_id=$1 AND idempotency_key=$2`, caseID, key).Scan(&actor, &hash, &action, &snapshot)
	if errors.Is(err, pgx.ErrNoRows) {
		return PaymentFinancialCompletionResult{}, false, nil
	}
	if err != nil {
		return PaymentFinancialCompletionResult{}, false, err
	}
	if actor != actorID || hash != paymentCompletionDigest(request) || action != request.Action {
		return PaymentFinancialCompletionResult{}, false, ErrConflict
	}
	var result PaymentFinancialCompletionResult
	if json.Unmarshal(snapshot, &result) != nil || result.CaseID != caseID || result.Case.ID != caseID || result.Proposal.CaseID != caseID || result.Proposal.ID == uuid.Nil {
		return result, false, ErrConflict
	}
	return result, true, nil
}

func loadPaymentCompletionProposal(ctx context.Context, source paymentRepairReader, caseID, proposalID uuid.UUID) (PaymentFinancialCompletionProposal, error) {
	var raw json.RawMessage
	var actor uuid.UUID
	err := source.QueryRow(ctx, `SELECT actor_id,after_snapshot FROM payment_anomaly_commands WHERE id=$1 AND payment_anomaly_case_id=$2 AND action=$3`, proposalID, caseID, paymentCompletionPropose).Scan(&actor, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return PaymentFinancialCompletionProposal{}, ErrConflict
	}
	if err != nil {
		return PaymentFinancialCompletionProposal{}, err
	}
	var saved PaymentFinancialCompletionResult
	if json.Unmarshal(raw, &saved) != nil || saved.CaseID != caseID || saved.Proposal.ID != proposalID || saved.Proposal.CaseID != caseID || saved.Proposal.ProposerID != actor || saved.Proposal.CaseVersion < 1 {
		return saved.Proposal, ErrConflict
	}
	return saved.Proposal, nil
}

// PaymentFinancialCompletion runs behind the existing current administrator
// permission middleware. Replay is read before any live provider call, so a
// response lost after commit never requires another external operation.
func (p *PaymentCoordinator) PaymentFinancialCompletion(ctx context.Context, caseID, actorID uuid.UUID, key, reason string, input PaymentFinancialCompletionInput, proposalID uuid.UUID) (PaymentFinancialCompletionResult, error) {
	key, reason = strings.TrimSpace(key), strings.TrimSpace(reason)
	input.ProofKind = strings.ToUpper(strings.TrimSpace(input.ProofKind))
	action := paymentCompletionPropose
	if proposalID != uuid.Nil {
		action = paymentCompletionConfirm
	}
	request := paymentCompletionRequest{Action: action, Input: input, ProposalID: proposalID, Reason: reason}
	if p == nil || p.repository == nil || caseID == uuid.Nil || actorID == uuid.Nil || len(key) < 8 || len(key) > 120 || len([]rune(reason)) < 10 || len([]rune(reason)) > 2000 {
		return PaymentFinancialCompletionResult{}, ErrConflict
	}
	if proposalID != uuid.Nil && (input.ProofKind != "" || input.ReferenceID != uuid.Nil) {
		return PaymentFinancialCompletionResult{}, ErrConflict
	}
	if replay, found, err := loadPaymentCompletionReplay(ctx, p.repository.pool, caseID, actorID, key, request); err != nil || found {
		return replay, err
	}
	proof := input
	if proposalID != uuid.Nil {
		proposal, err := loadPaymentCompletionProposal(ctx, p.repository.pool, caseID, proposalID)
		if err != nil {
			return PaymentFinancialCompletionResult{}, err
		}
		if proposal.ProposerID == actorID {
			return PaymentFinancialCompletionResult{}, ErrConflict
		}
		proof = proposal.Input
	}
	var query *paymentCompletionQuery
	if proof.ProofKind == "NO_FINANCIAL_EFFECT" {
		detail, err := p.repository.PaymentAnomalyDetail(ctx, caseID)
		if err != nil {
			return PaymentFinancialCompletionResult{}, err
		}
		if !paymentCompletionEligible(detail.Case) {
			return PaymentFinancialCompletionResult{}, ErrConflict
		}
		order, err := getPaymentOrderByID(ctx, p.repository.pool, *detail.Case.PaymentOrderID, false)
		if err != nil {
			return PaymentFinancialCompletionResult{}, err
		}
		capabilities := p.Capabilities()
		if !capabilities.OrderQuerySupported || capabilities.ProviderID != order.ProviderID || capabilities.Version != order.ProviderVersion {
			return PaymentFinancialCompletionResult{}, ErrConflict
		}
		query = &paymentCompletionQuery{}
		if err := p.repository.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&query.StartedAt); err != nil {
			return PaymentFinancialCompletionResult{}, err
		}
		query.Order, err = providerCall(ctx, p, "ORDER_QUERY", order.Channel, false, providerCallScope(order), func() (ProviderOrder, error) { return p.provider.Get(ctx, order.MerchantOrderNo) })
		if err != nil {
			return PaymentFinancialCompletionResult{}, errors.Join(ErrPaymentProvider, err)
		}
	}
	return p.repository.applyPaymentFinancialCompletion(ctx, caseID, actorID, key, request, query)
}

func (r *PostgresRepository) applyPaymentFinancialCompletion(ctx context.Context, caseID, actorID uuid.UUID, key string, request paymentCompletionRequest, query *paymentCompletionQuery) (PaymentFinancialCompletionResult, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return PaymentFinancialCompletionResult{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockTransactionKey(ctx, tx, "payment-anomaly.financial-completion", caseID.String()); err != nil {
		return PaymentFinancialCompletionResult{}, err
	}
	if replay, found, err := loadPaymentCompletionReplay(ctx, tx, caseID, actorID, key, request); err != nil || found {
		if err != nil {
			return replay, err
		}
		return replay, tx.Commit(ctx)
	}
	var orderID *uuid.UUID
	err = tx.QueryRow(ctx, `SELECT payment_order_id FROM payment_anomaly_cases WHERE id=$1`, caseID).Scan(&orderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return PaymentFinancialCompletionResult{}, ErrNotFound
	}
	if err != nil {
		return PaymentFinancialCompletionResult{}, err
	}
	if orderID == nil {
		return PaymentFinancialCompletionResult{}, ErrConflict
	}
	order, err := getPaymentOrderByID(ctx, tx, *orderID, true)
	if err != nil {
		return PaymentFinancialCompletionResult{}, err
	}
	item, found, err := scanPaymentAnomaly(tx.QueryRow(ctx, paymentAnomalySelect+` WHERE anomaly.id=$1 FOR UPDATE OF anomaly,manual`, caseID))
	if err != nil {
		return PaymentFinancialCompletionResult{}, err
	}
	if !found || !paymentCompletionEligible(item) || *item.PaymentOrderID != order.ID {
		return PaymentFinancialCompletionResult{}, ErrConflict
	}
	var manualStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM manual_review_cases WHERE id=$1`, caseID).Scan(&manualStatus); err != nil {
		return PaymentFinancialCompletionResult{}, err
	}
	if manualStatus != "LONG_TERM_PLAN" {
		return PaymentFinancialCompletionResult{}, ErrConflict
	}
	before, _ := json.Marshal(item)
	now := r.now()
	commandID := uuid.Must(uuid.NewV7())
	proposal := PaymentFinancialCompletionProposal{ID: commandID, CaseID: caseID, CaseVersion: item.Version, ProposerID: actorID, Input: request.Input, Reason: request.Reason, CreatedAt: now}
	if request.Action == paymentCompletionConfirm {
		proposal, err = loadPaymentCompletionProposal(ctx, tx, caseID, request.ProposalID)
		if err != nil {
			return PaymentFinancialCompletionResult{}, err
		}
		if proposal.ProposerID == actorID || proposal.CaseVersion != item.Version {
			return PaymentFinancialCompletionResult{}, ErrConflict
		}
	}
	facts, err := paymentCompletionFacts(ctx, tx, item, order, proposal.Input, query)
	if err != nil {
		return PaymentFinancialCompletionResult{}, err
	}
	if request.Action == paymentCompletionPropose {
		proposal.Facts = facts
		if query != nil {
			proposal.QueryStartedAt = &query.StartedAt
			proposal.ProviderSnapshot, _ = json.Marshal(query.Order)
		}
	} else {
		if facts.Digest != proposal.Facts.Digest {
			return PaymentFinancialCompletionResult{}, ErrConflict
		}
		financialStatus := "APPLIED"
		if proposal.Input.ProofKind == "NO_FINANCIAL_EFFECT" {
			financialStatus = "NONE"
		}
		command, err := tx.Exec(ctx, `UPDATE payment_anomaly_cases SET financial_action_status=$3,version=version+1,updated_at=$4 WHERE id=$1 AND version=$2 AND status='RESOLVED' AND resolution_code='ESCALATE_FINANCIAL_REPAIR' AND financial_action_status='PENDING'`, caseID, item.Version, financialStatus, now)
		if err != nil {
			return PaymentFinancialCompletionResult{}, err
		}
		if command.RowsAffected() != 1 {
			return PaymentFinancialCompletionResult{}, ErrConflict
		}
		command, err = tx.Exec(ctx, `UPDATE manual_review_cases SET status='RESOLVED',resolved_at=$2,updated_at=$2 WHERE id=$1 AND status='LONG_TERM_PLAN'`, caseID, now)
		if err != nil {
			return PaymentFinancialCompletionResult{}, err
		}
		if command.RowsAffected() != 1 {
			return PaymentFinancialCompletionResult{}, ErrConflict
		}
		if financialStatus == "NONE" {
			command, err = tx.Exec(ctx, `UPDATE payment_orders SET status='FAILED',fulfillment_status='PENDING',close_status='EXPIRED',payment_url=NULL,qr_data=NULL,reconciliation_next_at=NULL,reconciliation_lease_until=NULL,reconciliation_last_error=NULL,provider_status='FAILED',last_successful_query_at=$2,last_authoritative_unpaid_at=$2,updated_at=$3 WHERE id=$1 AND status='MANUAL_REVIEW' AND fulfillment_status='MANUAL_REVIEW'`, order.ID, query.StartedAt, now)
			if err != nil {
				return PaymentFinancialCompletionResult{}, err
			}
			if command.RowsAffected() != 1 {
				return PaymentFinancialCompletionResult{}, ErrConflict
			}
		}
		if _, err := projectPaymentOrderWithinTransaction(ctx, tx, order.ID, now); err != nil {
			return PaymentFinancialCompletionResult{}, err
		}
		item.FinancialActionStatus = financialStatus
		item.Version++
		item.UpdatedAt = now
	}
	result := PaymentFinancialCompletionResult{CaseID: caseID, Case: item, Proposal: proposal}
	after, _ := json.Marshal(result)
	_, err = tx.Exec(ctx, `INSERT INTO payment_anomaly_commands(id,payment_anomaly_case_id,actor_id,idempotency_key,request_hash,action,reason,before_snapshot,after_snapshot,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, commandID, caseID, actorID, key, paymentCompletionDigest(request), request.Action, request.Reason, before, after, now)
	if err != nil {
		return result, err
	}
	details := map[string]any{"proposal_id": proposal.ID, "proposer_id": proposal.ProposerID, "proof_kind": facts.ProofKind, "reference_id": facts.ReferenceID, "facts_digest": facts.Digest, "reason": request.Reason, "financial_action_status": item.FinancialActionStatus}
	if query != nil {
		details["query_started_at"] = query.StartedAt
		details["provider_snapshot"] = query.Order
	}
	if err := appendManualReviewEvent(ctx, tx, caseID, request.Action, &actorID, details, now); err != nil {
		return result, err
	}
	return result, tx.Commit(ctx)
}
