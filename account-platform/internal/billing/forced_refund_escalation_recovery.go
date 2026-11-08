package billing

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// CompleteEscalatedForcedRefund is the narrow recovery path for a forced
// refund that was escalated before its entitlement reversal completed. The
// original escalation owner is the first reviewer; a different actor must
// submit the completion command. The existing forced-refund financial writer
// and its CAS/receipt rules are shared within this one serializable transaction.
func (r *PostgresRepository) CompleteEscalatedForcedRefund(ctx context.Context, caseID, actorID uuid.UUID, idempotencyKey, reason string) (PaymentAnomalyCase, error) {
	idempotencyKey, reason = strings.TrimSpace(idempotencyKey), strings.TrimSpace(reason)
	if caseID == uuid.Nil || actorID == uuid.Nil || len(idempotencyKey) < 8 || len(idempotencyKey) > 120 || len([]rune(reason)) < 10 {
		return PaymentAnomalyCase{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return PaymentAnomalyCase{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockTransactionKey(ctx, tx, "payment-anomaly.forced-refund-completion", caseID.String()+":"+idempotencyKey); err != nil {
		return PaymentAnomalyCase{}, err
	}
	hash := paymentAnomalyCommandHash("COMPLETE_ESCALATED_FORCED_REFUND", reason)
	var savedHash string
	var savedActor uuid.UUID
	var snapshot json.RawMessage
	err = tx.QueryRow(ctx, `SELECT request_hash,actor_id,after_snapshot FROM payment_anomaly_commands WHERE payment_anomaly_case_id=$1 AND idempotency_key=$2`, caseID, idempotencyKey).Scan(&savedHash, &savedActor, &snapshot)
	if err == nil {
		if savedHash != hash || savedActor != actorID {
			return PaymentAnomalyCase{}, ErrConflict
		}
		var item PaymentAnomalyCase
		if json.Unmarshal(snapshot, &item) != nil || item.ID != caseID {
			return PaymentAnomalyCase{}, ErrConflict
		}
		return item, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return PaymentAnomalyCase{}, err
	}
	var forcedID, orderID *uuid.UUID
	err = tx.QueryRow(ctx, `SELECT forced_refund_case_id,payment_order_id FROM payment_anomaly_cases WHERE id=$1`, caseID).Scan(&forcedID, &orderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return PaymentAnomalyCase{}, ErrNotFound
	}
	if err != nil {
		return PaymentAnomalyCase{}, err
	}
	if forcedID == nil || orderID == nil {
		return PaymentAnomalyCase{}, ErrConflict
	}
	// Keep the standalone forced-refund writer's advisory gate before the
	// wallet, then follow the anomaly update's wallet/order/review order.
	if err := lockForcedRefundApplication(ctx, tx, *forcedID); err != nil {
		return PaymentAnomalyCase{}, err
	}
	order, err := getPaymentOrderByID(ctx, tx, *orderID, true)
	if err != nil {
		return PaymentAnomalyCase{}, err
	}
	item, found, err := scanPaymentAnomaly(tx.QueryRow(ctx, paymentAnomalySelect+` WHERE anomaly.id=$1 FOR UPDATE OF anomaly,manual`, caseID))
	if err != nil || !found {
		if err == nil {
			err = ErrNotFound
		}
		return PaymentAnomalyCase{}, err
	}
	if item.AnomalyType != "FORCED_REFUND" || item.Status != "RESOLVED" || item.ResolutionCode == nil || *item.ResolutionCode != "ESCALATE_FINANCIAL_REPAIR" || item.FinancialActionStatus != "PENDING" || item.ForcedRefundCaseID == nil || item.OwnerID == nil || *item.OwnerID == actorID {
		return PaymentAnomalyCase{}, ErrConflict
	}
	if !equalUUID(item.PaymentOrderID, orderID) || !equalUUID(item.ForcedRefundCaseID, forcedID) {
		return PaymentAnomalyCase{}, ErrConflict
	}
	var manualStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM manual_review_cases WHERE id=$1`, caseID).Scan(&manualStatus); err != nil {
		return PaymentAnomalyCase{}, err
	}
	if manualStatus != "LONG_TERM_PLAN" {
		return PaymentAnomalyCase{}, ErrConflict
	}
	var linkedOrderID, linkedTransactionID, linkedEvidenceID, linkedWalletID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT forced.payment_order_id,forced.payment_transaction_id,forced.payment_evidence_id,forced.wallet_id FROM forced_refund_cases forced WHERE forced.id=$1 AND forced.manual_review_case_id=$2`, *item.ForcedRefundCaseID, caseID).Scan(&linkedOrderID, &linkedTransactionID, &linkedEvidenceID, &linkedWalletID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PaymentAnomalyCase{}, ErrConflict
		}
		return PaymentAnomalyCase{}, err
	}
	if item.PaymentTransactionID == nil || item.PaymentEvidenceID == nil || order.ID != linkedOrderID || order.WalletID != linkedWalletID || *item.PaymentTransactionID != linkedTransactionID || *item.PaymentEvidenceID != linkedEvidenceID {
		return PaymentAnomalyCase{}, ErrConflict
	}
	before, _ := json.Marshal(item)
	// Financial facts and anomaly completion must use the same snapshot and
	// commit atomically. An independent inner commit invalidates this outer
	// serializable snapshot before the forced-row terminal update below.
	if _, err := r.applyReviewedForcedRefundInTx(ctx, tx, *item.ForcedRefundCaseID, true); err != nil {
		return PaymentAnomalyCase{}, err
	}
	if command, err := tx.Exec(ctx, `UPDATE forced_refund_cases SET status='RESOLVED',resolution_code='CONFIRM_FORCED_REFUND',resolution_reason=CASE WHEN status='REVIEW_REQUIRED' THEN $2 ELSE resolution_reason END,resolved_at=COALESCE(resolved_at,clock_timestamp()),version=CASE WHEN status='REVIEW_REQUIRED' THEN version+1 ELSE version END WHERE id=$1 AND (status='REVIEW_REQUIRED' OR (status='RESOLVED' AND resolution_code='CONFIRM_FORCED_REFUND'))`, *item.ForcedRefundCaseID, reason); err != nil || command.RowsAffected() != 1 {
		if err == nil {
			err = ErrConflict
		}
		return PaymentAnomalyCase{}, err
	}
	command, err := tx.Exec(ctx, `UPDATE payment_anomaly_cases SET financial_action_status='APPLIED',version=version+1,updated_at=clock_timestamp() WHERE id=$1 AND status='RESOLVED' AND resolution_code='ESCALATE_FINANCIAL_REPAIR' AND financial_action_status='PENDING' AND version=$2`, caseID, item.Version)
	if err != nil || command.RowsAffected() != 1 {
		if err == nil {
			err = ErrConflict
		}
		return PaymentAnomalyCase{}, err
	}
	if command, err := tx.Exec(ctx, `UPDATE manual_review_cases SET status='RESOLVED',resolved_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=$1 AND status='LONG_TERM_PLAN'`, caseID); err != nil || command.RowsAffected() != 1 {
		if err == nil {
			err = ErrConflict
		}
		return PaymentAnomalyCase{}, err
	}
	if item.PaymentOrderID != nil {
		if _, err := tx.Exec(ctx, `UPDATE payment_orders payment SET
chargeback_summary=chargeback_summary||jsonb_build_object('review_count',(SELECT count(*) FROM chargeback_cases WHERE payment_order_id=$1 AND status='REVIEW_REQUIRED')),
refund_summary=refund_summary||jsonb_build_object('forced_review_count',(SELECT count(*) FROM forced_refund_cases WHERE payment_order_id=$1 AND status='REVIEW_REQUIRED')),
attention_summary=CASE
 WHEN EXISTS(SELECT 1 FROM receivables WHERE payment_order_id=$1 AND outstanding_microcredits>0) THEN 'RECEIVABLE_OPEN'
 WHEN EXISTS(SELECT 1 FROM payment_anomaly_cases WHERE payment_order_id=$1 AND status='OPEN')
   OR EXISTS(SELECT 1 FROM payment_transactions WHERE payment_order_id=$1 AND (status='REVIEW_REQUIRED' OR fulfillment_status='REVIEW_REQUIRED'))
   OR EXISTS(SELECT 1 FROM refunds WHERE payment_order_id=$1 AND status='MANUAL_REVIEW') THEN 'REVIEW_REQUIRED'
 ELSE 'NORMAL' END,
summary_version=summary_version+1,updated_at=clock_timestamp() WHERE payment.id=$1`, *item.PaymentOrderID); err != nil {
			return PaymentAnomalyCase{}, err
		}
	}
	item.FinancialActionStatus = "APPLIED"
	item.Version++
	item.UpdatedAt = r.now()
	after, _ := json.Marshal(item)
	if _, err := tx.Exec(ctx, `INSERT INTO payment_anomaly_commands(id,payment_anomaly_case_id,actor_id,idempotency_key,request_hash,action,reason,before_snapshot,after_snapshot,created_at) VALUES($1,$2,$3,$4,$5,'COMPLETE_ESCALATED_FORCED_REFUND',$6,$7,$8,$9)`, uuid.Must(uuid.NewV7()), caseID, actorID, idempotencyKey, hash, reason, before, after, r.now()); err != nil {
		return PaymentAnomalyCase{}, err
	}
	if err := appendManualReviewEvent(ctx, tx, caseID, "COMPLETE_ESCALATED_FORCED_REFUND", &actorID, map[string]any{"reason": reason, "financial_action_status": "APPLIED"}, r.now()); err != nil {
		return PaymentAnomalyCase{}, err
	}
	return item, tx.Commit(ctx)
}
