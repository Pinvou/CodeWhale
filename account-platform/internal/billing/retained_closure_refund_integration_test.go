package billing

import (
	"testing"

	"github.com/google/uuid"
)

// Reuses the inherited guarded UUID/public production-migration fixture. Provider
// facts are controlled; this proves the actual closure assessment predicate and
// refund state transitions, not live provider collection or whole account closure.
func TestRetainedClosureRefundTerminalAssessment(t *testing.T) {
	for _, status := range []string{"REQUESTED", "FAILED", "SUCCESS"} {
		t.Run(status, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			coordinator, order, transaction, pack := retainedFinanceOrder(t, r, ctx, 1000000, true)
			coordinator.AllowNormalRefundCreation(true)
			refund, created, err := coordinator.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, uuid.NewString(), "controlled whole legacy refund")
			if err != nil || !created {
				t.Fatal("real refund producer failed", err)
			}
			if status != "REQUESTED" {
				got, _, err := coordinator.finishRefund(ctx, refund, ProviderRefund{MerchantRefundNo: refund.MerchantRefundNo, AmountFen: refund.AmountFen, Status: status, ChannelRefundID: "controlled-refund"}, false)
				if err != nil || (status == "FAILED" && got.Status != "FAILED") || (status == "SUCCESS" && got.Status != "SUCCEEDED") {
					t.Fatal("real refund terminal transition failed", err)
				}
			}
			assessment, err := r.AccountClosureAssessment(ctx, order.RequesterID)
			if err != nil {
				t.Fatal("real closure assessment failed", err)
			}
			count := 0
			for _, blocker := range assessment.Blockers {
				if blocker.Code == "REFUND_UNRESOLVED" {
					count++
				}
			}
			want := 0
			if status == "REQUESTED" {
				want = 1
			}
			if count != want {
				t.Fatalf("terminal=%s refund blockers=%d want=%d", status, count, want)
			}
		})
	}
}
