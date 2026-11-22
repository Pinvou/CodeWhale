package billing

import (
	"testing"

	"github.com/google/uuid"
)

func TestAccountClosurePreservesUnknownForcedRefundEvidence(t *testing.T) {
	for _, outcome := range []string{"pending", "legacy", "confirmed", "dismissed"} {
		t.Run(outcome, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			_, order, transaction, _ := retainedFinanceOrder(t, r, ctx, 10, true)
			item, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "closure-source-"+outcome, 50, nil)
			if err != nil {
				t.Fatal(err)
			}
			actor := uuid.New()
			switch outcome {
			case "legacy":
				// Reproduce the preserved legacy disposition; no receipt is invented.
				_, err = r.pool.Exec(ctx, `UPDATE forced_refund_cases SET status='RESOLVED',resolution_code=NULL,version=version+1,resolved_at=now() WHERE id=$1`, item.ID)
			case "confirmed":
				if _, err = r.ApplyReviewedForcedRefund(ctx, item.ID); err == nil {
					retainedClaimAnomaly(t, r, ctx, *item.ManualReviewCaseID, actor)
					_, err = r.ResolvePaymentAnomaly(ctx, *item.ManualReviewCaseID, actor, uuid.NewString(), "CONFIRM_FORCED_REFUND", "controlled trusted provider evidence", "APPLIED")
				}
			case "dismissed":
				retainedClaimAnomaly(t, r, ctx, *item.ManualReviewCaseID, actor)
				_, err = r.ResolvePaymentAnomaly(ctx, *item.ManualReviewCaseID, actor, uuid.NewString(), "DISMISS", "controlled trusted case disproved", "NONE")
			}
			if err != nil {
				t.Fatal(err)
			}
			var payer uuid.UUID
			if err = r.pool.QueryRow(ctx, `SELECT payer_user_id FROM payment_orders WHERE id=$1`, order.ID).Scan(&payer); err != nil {
				t.Fatal(err)
			}
			assessment, err := r.AccountClosureAssessment(ctx, payer)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, blocker := range assessment.Blockers {
				found = found || blocker.Code == "FORCED_REFUND_UNRESOLVED"
			}
			if found != (outcome == "pending" || outcome == "legacy") {
				t.Fatalf("outcome=%s unresolved=%v", outcome, found)
			}
		})
	}
}

func TestAccountClosureRequiresAuthoritativeChargebackTerminal(t *testing.T) {
	for _, outcome := range []string{"pending", "unproven-terminal", "confirmed", "dismissed"} {
		t.Run(outcome, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			_, order, transaction, _ := retainedFinanceOrder(t, r, ctx, 10, true)
			item, err := r.RecordConfirmedChargebackForReview(ctx, transaction, "closure-chargeback-"+outcome, nil)
			if err != nil {
				t.Fatal(err)
			}
			actor := uuid.New()
			switch outcome {
			case "unproven-terminal":
				// A status label alone cannot replace immutable financial evidence.
				_, err = r.pool.Exec(ctx, `UPDATE chargeback_cases SET status='CONFIRMED',version=version+1 WHERE id=$1`, item.ID)
			case "confirmed":
				if _, err = r.ApplyConfirmedChargeback(ctx, item.PaymentEvidenceID, item.ProviderChargebackID); err == nil {
					retainedClaimAnomaly(t, r, ctx, *item.ManualReviewCaseID, actor)
					_, err = r.ResolvePaymentAnomaly(ctx, *item.ManualReviewCaseID, actor, uuid.NewString(), "CONFIRM_CHARGEBACK", "controlled trusted provider evidence", "APPLIED")
				}
			case "dismissed":
				retainedClaimAnomaly(t, r, ctx, *item.ManualReviewCaseID, actor)
				_, err = r.ResolvePaymentAnomaly(ctx, *item.ManualReviewCaseID, actor, uuid.NewString(), "DISMISS", "controlled trusted case disproved", "NONE")
			}
			if err != nil {
				t.Fatal(err)
			}
			var payer uuid.UUID
			if err = r.pool.QueryRow(ctx, `SELECT payer_user_id FROM payment_orders WHERE id=$1`, order.ID).Scan(&payer); err != nil {
				t.Fatal(err)
			}
			assessment, err := r.AccountClosureAssessment(ctx, payer)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, blocker := range assessment.Blockers {
				found = found || blocker.Code == "CHARGEBACK_UNRESOLVED"
			}
			if found != (outcome == "pending" || outcome == "unproven-terminal") {
				t.Fatalf("outcome=%s unresolved=%v", outcome, found)
			}
		})
	}
}
