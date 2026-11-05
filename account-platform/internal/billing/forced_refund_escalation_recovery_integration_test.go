package billing

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestEscalatedForcedRefundRequiresSecondActorAndReplays(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	_, _, transaction, _ := retainedFinanceOrder(t, r, ctx, 1000000, true)
	forced, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "escalated-forced-refund", 50, nil)
	if err != nil || forced.ManualReviewCaseID == nil {
		t.Fatal("forced refund review was not created", err)
	}
	firstActor, secondActor := uuid.New(), uuid.New()
	if _, err = r.ClaimPaymentAnomaly(ctx, *forced.ManualReviewCaseID, firstActor, "escalated-claim-key"); err != nil {
		t.Fatal(err)
	}
	if _, err = r.ResolvePaymentAnomaly(ctx, *forced.ManualReviewCaseID, firstActor, "escalated-resolution-key", "ESCALATE_FINANCIAL_REPAIR", "controlled forced refund escalation", "PENDING"); err != nil {
		t.Fatal(err)
	}
	if _, err = r.CompleteEscalatedForcedRefund(ctx, *forced.ManualReviewCaseID, firstActor, "completion-key", "independent forced refund completion"); !errors.Is(err, ErrConflict) {
		t.Fatal("the escalation owner was allowed to provide both approvals", err)
	}
	completed, err := r.CompleteEscalatedForcedRefund(ctx, *forced.ManualReviewCaseID, secondActor, "completion-key", "independent forced refund completion")
	if err != nil || completed.FinancialActionStatus != "APPLIED" {
		t.Fatal("second actor did not complete escalated forced refund", completed, err)
	}
	replay, err := r.CompleteEscalatedForcedRefund(ctx, *forced.ManualReviewCaseID, secondActor, "completion-key", "independent forced refund completion")
	if err != nil || replay.FinancialActionStatus != "APPLIED" {
		t.Fatal("completion replay was not stable", replay, err)
	}
	var forcedStatus string
	var journals int
	if err = r.pool.QueryRow(ctx, `SELECT status FROM forced_refund_cases WHERE id=$1`, forced.ID).Scan(&forcedStatus); err != nil || forcedStatus != "RESOLVED" {
		t.Fatal("forced refund case did not reach its terminal state", forcedStatus, err)
	}
	if err = r.pool.QueryRow(ctx, `SELECT count(*) FROM journal_entries WHERE business_reference=$1`, "forced-refund:"+forced.ID.String()).Scan(&journals); err != nil || journals != 1 {
		t.Fatal("completion created an unexpected number of financial journals", journals, err)
	}
}
