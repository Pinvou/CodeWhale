package billing

import (
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
)

// The original public-migration UUID fixture and actual owner executor are used.
// Payment provider facts are controlled; this is not live collection, independent
// approval/issuer delivery, resource activation or deployment acceptance.
func TestReconciliationFinancialRepairRelatedFactsPostgres(t *testing.T) {
	if os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL") == "" {
		t.Skip("explicit disposable Billing PostgreSQL configuration is required")
	}
	r, ctx := retainedFinanceRepository(t)
	v := reconciliationFixtureActivation(t, ctx, r.pool)("ACTIVE")
	plan := func(t *testing.T, f reconciliationRepairFixture) ReconciliationFinancialRepairPlan {
		t.Helper()
		p, err := reconciliationFinancialRepairPlanFrom(ctx, r.pool, ReconciliationFinancialRepairCommandInput{
			ReconciliationCaseID: f.CaseID, PaymentEvidenceID: f.EvidenceID, BusinessDate: f.BusinessDate,
		}, 1, 1, v)
		if err != nil {
			t.Fatal("original related owner plan", err)
		}
		return p
	}

	for _, change := range []string{"unrelated-wallet-version", "unrelated-successful-payment"} {
		t.Run(change, func(t *testing.T) {
			// Create the unrelated wallet before freezing, so this branch tests
			// its version change rather than merely a new wallet's table count.
			wallet, _, err := r.EnsureWallet(ctx, "PERSONAL", uuid.New())
			if err != nil {
				t.Fatal(err)
			}
			f := seedProviderPaidInternalMissing(t, ctx, r, change)
			frozenPlan := plan(t, f)
			input := reconciliationFrozenInput(frozenPlan, v)
			if change == "unrelated-wallet-version" {
				tag, err := r.pool.Exec(ctx, `UPDATE wallets SET version=version+1,updated_at=now() WHERE id=$1`, wallet.ID)
				if err != nil || tag.RowsAffected() != 1 {
					t.Fatal("unrelated wallet update not applied", err)
				}
			} else {
				// This existing fixture calls the actual successful-payment owner
				// merge in a committed transaction, including its journal/package
				// effects. Only its provider response and lawful source seed are
				// controlled, without any external provider request.
				_, order, transactionID, packageID := retainedFinanceOrder(t, r, ctx, 1000000, true)
				if order.MerchantOrderNo == f.MerchantOrderNo || transactionID == uuid.Nil || packageID == uuid.Nil {
					t.Fatal("unrelated successful payment owner effect not reached")
				}
			}
			current := plan(t, f)
			if current.FactsDigest != frozenPlan.FactsDigest {
				t.Error("unrelated activity invalidated this closure's frozen owner facts")
			}
			// Capture after the unrelated activity and before this closure: the
			// full original fourteen-table financial assertion remains intact.
			before := reconciliationEffectSnapshot(t, ctx, r, false)
			result, err := r.ApplyReconciliationFinancialRepair(ctx, input)
			if err != nil || result.Status != "SUCCEEDED" || result.CaseID != f.CaseID ||
				result.FactsDigest != frozenPlan.FactsDigest || result.LedgerEffect != "NONE" || result.RecheckCaseCount != 0 {
				t.Fatal("original frozen owner execution rejected unrelated activity", err)
			}
			if reconciliationEffectSnapshot(t, ctx, r, false) != before {
				t.Fatal("closure changed financial facts after unrelated activity")
			}
			var caseStatus, manualStatus string
			var caseVersion, receipts int64
			if err := r.pool.QueryRow(ctx, `SELECT c.status,c.version,m.status,
 (SELECT count(*) FROM reconciliation_repair_cases WHERE reconciliation_case_id=c.id)
 FROM reconciliation_cases c JOIN manual_review_cases m ON m.id=c.manual_review_case_id WHERE c.id=$1`, f.CaseID).
				Scan(&caseStatus, &caseVersion, &manualStatus, &receipts); err != nil ||
				caseStatus != "RESOLVED" || caseVersion != frozenPlan.CaseVersion+1 || manualStatus != "RESOLVED" || receipts != 1 {
				t.Fatal("related case/manual-review/immutable receipt closure incomplete", err)
			}
			// A committed result remains first: no second effect after fresh
			// mutable owner rows have naturally changed to terminal state.
			allBefore := reconciliationEffectSnapshot(t, ctx, r, true)
			replayed, err := r.ApplyReconciliationFinancialRepair(ctx, input)
			if err != nil || replayed != result || reconciliationEffectSnapshot(t, ctx, r, true) != allBefore {
				t.Fatal("matching result-first receipt or zero-effect replay changed", err)
			}
		})
	}

	for _, change := range []string{"related-case-version", "related-manual-review-row"} {
		t.Run(change, func(t *testing.T) {
			f := seedProviderPaidInternalMissing(t, ctx, r, change)
			input := reconciliationFrozenInput(plan(t, f), v)
			statement := `UPDATE reconciliation_cases SET version=version+1 WHERE id=$1`
			if change == "related-manual-review-row" {
				statement = `UPDATE manual_review_cases SET updated_at=updated_at+interval '1 second' WHERE id=$1`
			}
			tag, err := r.pool.Exec(ctx, statement, f.CaseID)
			if err != nil || tag.RowsAffected() != 1 {
				t.Fatal("related owner drift not applied", err)
			}
			before := reconciliationEffectSnapshot(t, ctx, r, true)
			_, err = r.ApplyReconciliationFinancialRepair(ctx, input)
			if !errors.Is(err, ErrApprovalSnapshotMismatch) {
				t.Fatal("related owner drift accepted", err)
			}
			if reconciliationEffectSnapshot(t, ctx, r, true) != before {
				t.Fatal("related drift rejection changed financial, case or audit facts")
			}
		})
	}
}
