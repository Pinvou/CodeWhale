package billing

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPaymentSharedProviderTransactionRejectsOtherOrderAndRepairPostgres(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	c1, first, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
	firstProvider := c1.provider.(*retainedFinancialProvider)
	late := first.ExpiresAt.Add(time.Minute)
	firstProvider.result.PaidAt = &late
	if _, err := c1.applyQueriedPayment(ctx, first.ID, firstProvider.result, false); err != nil {
		t.Fatal(err)
	}
	var fulfillment string
	if err := r.pool.QueryRow(ctx, `SELECT fulfillment_status FROM payment_transactions WHERE payment_order_id=$1`, first.ID).Scan(&fulfillment); err != nil || fulfillment != "REVIEW_REQUIRED" {
		t.Fatalf("original transaction must be unfulfilled review: %s %v", fulfillment, err)
	}
	c2, second, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
	secondProvider := c2.provider.(*retainedFinancialProvider)
	// All provider facts match the second order; only the money movement's
	// identity collides with the first order's real persisted transaction.
	secondProvider.result.ChannelTransactionID = firstProvider.result.ChannelTransactionID
	if _, err := c2.SyncOrder(ctx, second.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-order payment accepted: %v", err)
	}
	review := secondProvider.result
	review.Status = "REVIEW_REQUIRED"
	if err := c2.recordProviderObservation(ctx, second.ID, "QUERY", review); !errors.Is(err, ErrConflict) {
		t.Fatalf("create real review prerequisite: %v", err)
	}
	if _, err := c2.RepairReviewedOrder(ctx, second.ID, uuid.New(), "controlled cross-order evidence rejection"); !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-order repair accepted: %v", err)
	}
	var anomalies, queryEvidence, repairEvidence, packages, repairs int
	var posted int64
	if err := r.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM payment_anomaly_cases a JOIN manual_review_cases m ON m.id=a.id WHERE a.payment_order_id=$1 AND a.anomaly_type='PAYMENT_FACT_CONFLICT' AND m.severity='P0'),
 (SELECT count(*) FROM payment_evidence WHERE payment_order_id=$1 AND evidence_type='QUERY' AND merge_result='REVIEW_REQUIRED' AND payment_transaction_id IS NULL AND provider_transaction_id=$3),
 (SELECT count(*) FROM payment_evidence WHERE payment_order_id=$1 AND evidence_type='MANUAL_REPAIR' AND merge_result='REVIEW_REQUIRED' AND payment_transaction_id IS NULL),
 (SELECT count(*) FROM entitlement_packages WHERE payment_order_id IN ($1,$2)),
 (SELECT count(*) FROM payment_order_repairs WHERE payment_order_id=$1),
 (SELECT posted_microcredits FROM wallets WHERE id=$4)`, second.ID, first.ID, *firstProvider.result.ChannelTransactionID, second.WalletID).Scan(&anomalies, &queryEvidence, &repairEvidence, &packages, &repairs, &posted); err != nil {
		t.Fatal(err)
	}
	if anomalies != 2 || queryEvidence < 1 || repairEvidence != 1 || packages != 0 || repairs != 0 || posted != 0 {
		t.Fatalf("conflict not preserved: anomalies=%d query=%d repair=%d packages=%d receipts=%d posted=%d", anomalies, queryEvidence, repairEvidence, packages, repairs, posted)
	}
}
