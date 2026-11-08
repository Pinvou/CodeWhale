package billing

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Reuse the real payment, usage, anomaly and financial writers: the second
// plan is prepared after the first case completes, so a stale wallet version
// cannot mask a missing payment-source budget check.
func TestChargebackFinancialRepairSourceBudgetPostgres(t *testing.T) {
	for _, firstOutcome := range []string{"financial-repair", "confirmed-chargeback", "dismissed-no-effect"} {
		t.Run(firstOutcome, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			_, order, transaction, _ := retainedFinanceOrder(t, r, ctx, 1000000, true)
			usageID := uuid.New()
			hold, err := r.Reserve(ctx, usageID, order.WalletID, 1000000, nil, r.now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.Commit(ctx, usageID, hold.ID, 1000000, map[string]any{"controlled": true}); err != nil {
				t.Fatal(err)
			}
			activation := chargebackFixtureActivation(t, ctx, r.pool)("ACTIVE")
			first, err := r.RecordConfirmedChargebackForReview(ctx, transaction, "source-budget-first-"+uuid.NewString(), []byte(`{"controlled":true}`))
			if err != nil || first.ManualReviewCaseID == nil {
				t.Fatalf("first original evidence: %+v %v", first, err)
			}
			actor, key := uuid.New(), uuid.NewString()
			retainedClaimAnomaly(t, r, ctx, *first.ManualReviewCaseID, actor)
			const reason = "controlled source budget regression"
			switch firstOutcome {
			case "financial-repair":
				if _, err := retainedResolveClaimedAnomaly(t, r, ctx, *first.ManualReviewCaseID, actor, key, "ESCALATE_FINANCIAL_REPAIR", reason, "PENDING"); err != nil {
					t.Fatal(err)
				}
				plan, err := chargebackFinancialRepairPlanFrom(ctx, r.pool, ChargebackFinancialRepairCommandInput{ChargebackCaseID: first.ID, PaymentEvidenceID: first.PaymentEvidenceID}, 1, 1, activation)
				if err != nil {
					t.Fatal(err)
				}
				if result, err := r.ApplyChargebackFinancialRepair(ctx, chargebackFrozenInput(plan, activation)); err != nil || result.BaseShortfallMicrocredits != 1000000 {
					t.Fatalf("first repair: %+v %v", result, err)
				}
			case "confirmed-chargeback":
				if _, _, err := r.PreparePaymentAnomalyResolution(ctx, *first.ManualReviewCaseID, actor, key, "CONFIRM_CHARGEBACK", reason); err != nil {
					t.Fatal(err)
				}
				if _, err := r.ApplyConfirmedChargeback(ctx, first.PaymentEvidenceID, first.ProviderChargebackID); err != nil {
					t.Fatal(err)
				}
				if _, err := retainedResolveClaimedAnomaly(t, r, ctx, *first.ManualReviewCaseID, actor, key, "CONFIRM_CHARGEBACK", reason, "APPLIED"); err != nil {
					t.Fatal(err)
				}
			case "dismissed-no-effect":
				if _, err := retainedResolveClaimedAnomaly(t, r, ctx, *first.ManualReviewCaseID, actor, key, "DISMISS", reason, "NONE"); err != nil {
					t.Fatal(err)
				}
			}
			second, err := r.RecordConfirmedChargebackForReview(ctx, transaction, "source-budget-second-"+uuid.NewString(), []byte(`{"controlled":true}`))
			if err != nil || second.ManualReviewCaseID == nil || second.ID == first.ID {
				t.Fatalf("distinct second original evidence: %+v %v", second, err)
			}
			actor, key = uuid.New(), uuid.NewString()
			retainedClaimAnomaly(t, r, ctx, *second.ManualReviewCaseID, actor)
			if _, err := retainedResolveClaimedAnomaly(t, r, ctx, *second.ManualReviewCaseID, actor, key, "ESCALATE_FINANCIAL_REPAIR", reason, "PENDING"); err != nil {
				t.Fatal(err)
			}
			target := ChargebackFinancialRepairCommandInput{ChargebackCaseID: second.ID, PaymentEvidenceID: second.PaymentEvidenceID}
			before := chargebackEffectSnapshot(t, ctx, r)
			plan, planErr := chargebackFinancialRepairPlanFrom(ctx, r.pool, target, 1, 1, activation)
			_, proofErr := r.ChargebackFinancialRepairProofInput(ctx, target)
			if after := chargebackEffectSnapshot(t, ctx, r); after != before {
				t.Fatal("planning or proof read changed financial facts")
			}
			if firstOutcome != "dismissed-no-effect" {
				if !errors.Is(planErr, ErrApprovalSnapshotMismatch) || !errors.Is(proofErr, ErrApprovalSnapshotMismatch) {
					t.Errorf("same payment already recovered principal: plan=%v proof=%v", planErr, proofErr)
				}
				var debt int64
				if err := r.pool.QueryRow(ctx, `SELECT COALESCE(sum(original_microcredits),0)::bigint FROM receivables WHERE payment_transaction_id=$1`, transaction).Scan(&debt); err != nil || debt != 1000000 {
					t.Fatalf("one principal disposition required: debt=%d err=%v", debt, err)
				}
				return
			}
			if planErr != nil || proofErr != nil {
				t.Fatalf("verified zero-effect dismissal must remain eligible: plan=%v proof=%v", planErr, proofErr)
			}
			input := chargebackFrozenInput(plan, activation)
			result, err := r.ApplyChargebackFinancialRepair(ctx, input)
			if err != nil || result.BaseShortfallMicrocredits != 1000000 {
				t.Fatalf("second case after zero-effect dismissal: %+v %v", result, err)
			}
			applied := chargebackEffectSnapshot(t, ctx, r)
			if replay, err := r.ApplyChargebackFinancialRepair(ctx, input); err != nil || replay != result {
				t.Fatalf("exact financial receipt replay: %+v %v", replay, err)
			}
			if after := chargebackEffectSnapshot(t, ctx, r); after != applied {
				t.Fatal("receipt replay changed financial facts")
			}
		})
	}
}
