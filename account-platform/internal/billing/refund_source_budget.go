package billing

import (
	"context"

	"github.com/google/uuid"
)

type forcedRefundBudget struct{ cash, base, bonus int64 }

// Called under the wallet/order/source-package gate. Current balance is never
// evidence of unused cash budget; even repaid receivables retain their original
// BASE disposition. Legacy or mixed normal-refund allocations require review.
func readForcedRefundBudget(ctx context.Context, q queryable, transactionID, packageID, orderID, walletID, excludingForcedID, excludingChargebackID uuid.UUID) (forcedRefundBudget, error) {
	var budget forcedRefundBudget
	var blocked bool
	err := q.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM refunds source WHERE payment_transaction_id=$1 AND (status IN ('REQUESTED','MANUAL_REVIEW','SUCCEEDED') OR EXISTS(SELECT 1 FROM journal_entries WHERE entry_type='REFUND' AND business_reference='refund:'||source.id::text)))
 OR EXISTS(SELECT 1 FROM chargeback_cases source WHERE source.payment_transaction_id=$1 AND source.id<>$2 AND NOT COALESCE(source.status='DISMISSED' AND source.resolution_code='DISMISS' AND EXISTS(SELECT 1 FROM payment_anomaly_cases anomaly JOIN payment_anomaly_commands command ON command.payment_anomaly_case_id=anomaly.id AND command.action='DISMISS' WHERE anomaly.id=source.manual_review_case_id AND anomaly.chargeback_case_id=source.id AND anomaly.status='RESOLVED' AND anomaly.resolution_code='DISMISS' AND anomaly.financial_action_status='NONE'),false))`, transactionID, excludingChargebackID).Scan(&blocked)
	if err != nil {
		return budget, err
	}
	if blocked {
		return budget, ErrConflict
	}
	err = q.QueryRow(ctx, `WITH sources AS (
 SELECT source.*,debt.original_microcredits debt_amount,
 EXISTS(SELECT 1 FROM journal_entries WHERE entry_type='FORCED_REFUND' AND business_reference='forced-refund:'||source.id::text) applied,
 CASE WHEN source.receivable_id IS NULL THEN true ELSE debt.id IS NOT NULL AND debt.wallet_id=$4 AND debt.payment_transaction_id=$1 AND debt.payment_order_id=$3 AND debt.entitlement_package_id=$2 AND debt.source_type='FORCED_REFUND' AND debt.source_reference='forced-refund:'||source.id::text END debt_valid,
 EXISTS(SELECT 1 FROM payment_anomaly_cases anomaly JOIN payment_anomaly_commands command ON command.payment_anomaly_case_id=anomaly.id AND command.action='DISMISS' WHERE anomaly.id=source.manual_review_case_id AND anomaly.forced_refund_case_id=source.id AND anomaly.status='RESOLVED' AND anomaly.resolution_code='DISMISS' AND anomaly.financial_action_status='NONE') dismissed
 FROM forced_refund_cases source LEFT JOIN receivables debt ON debt.id=source.receivable_id WHERE source.payment_transaction_id=$1 AND source.id<>$5
 ) SELECT COALESCE(sum(amount_fen) FILTER(WHERE applied),0)::bigint,
 COALESCE(sum(revoked_base_microcredits::numeric+COALESCE(debt_amount,0)) FILTER(WHERE applied),0)::bigint,
 COALESCE(sum(revoked_bonus_microcredits::numeric) FILTER(WHERE applied),0)::bigint,
 COALESCE(bool_or(payment_order_id<>$3 OR entitlement_package_id<>$2 OR wallet_id<>$4
 OR (applied AND (version<=1 OR NOT COALESCE(debt_valid,false) OR NOT COALESCE(status='REVIEW_REQUIRED' OR (status='RESOLVED' AND resolution_code='CONFIRM_FORCED_REFUND'),false)))
 OR (NOT applied AND (receivable_id IS NOT NULL OR revoked_base_microcredits<>0 OR revoked_bonus_microcredits<>0 OR (version>1 AND NOT (status='RESOLVED' AND resolution_code='DISMISS' AND dismissed)) OR (status='RESOLVED' AND NOT COALESCE(resolution_code='DISMISS' AND dismissed,false))))),false)
 FROM sources`, transactionID, packageID, orderID, walletID, excludingForcedID).Scan(&budget.cash, &budget.base, &budget.bonus, &blocked)
	if err != nil {
		return budget, err
	}
	if blocked || budget.cash < 0 || budget.base < 0 || budget.bonus < 0 {
		return budget, ErrConflict
	}
	return budget, nil
}
