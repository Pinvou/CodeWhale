package billing

import (
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"math"
	"testing"
	"time"
)

func TestForcedRefundCumulativeCashAndRights(t *testing.T) {
	for _, tc := range []struct {
		name    string
		credit  int64
		cash    []int64
		revoked []int64
	}{
		{"full_overbudget", 1000000, []int64{100, 100}, []int64{1000000, -1}},
		{"cumulative_rounding", 10, []int64{33, 33, 34}, []int64{4, 3, 3}},
		{"zero_rights_receipt", 1, []int64{1, 1}, []int64{1, 0}},
		{"max_int", math.MaxInt64, []int64{50, 50}, []int64{math.MaxInt64/2 + 1, math.MaxInt64 / 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			_, order, transaction, _ := retainedFinanceOrder(t, r, ctx, tc.credit, true)
			var total int64
			var successful int
			for i, cash := range tc.cash {
				item, err := r.RecordAndFreezeForcedRefund(ctx, transaction, tc.name+string(rune('A'+i)), cash, nil)
				if err != nil {
					t.Fatal("provider evidence must remain recordable", err)
				}
				applied, err := r.ApplyReviewedForcedRefund(ctx, item.ID)
				if tc.revoked[i] < 0 {
					if !errors.Is(err, ErrConflict) {
						t.Fatal("excess cumulative cash accepted", err)
					}
					var status string
					var version int64
					if err = r.pool.QueryRow(ctx, `SELECT status,version FROM forced_refund_cases WHERE id=$1`, item.ID).Scan(&status, &version); err != nil || status != "REVIEW_REQUIRED" || version != 1 {
						t.Fatal("rejected evidence changed", status, version, err)
					}
					continue
				}
				if err != nil || applied.RevokedBase != tc.revoked[i] || applied.ReceivableID != nil {
					t.Fatal("wrong cumulative rights", applied, err)
				}
				replay, err := r.ApplyReviewedForcedRefund(ctx, item.ID)
				if err != nil || replay.RevokedBase != applied.RevokedBase {
					t.Fatal("unstable replay", err)
				}
				total += applied.RevokedBase
				successful++
				var postings int
				if err = r.pool.QueryRow(ctx, `SELECT count(*) FROM ledger_postings p JOIN journal_entries j ON j.id=p.journal_entry_id WHERE j.business_reference=$1`, "forced-refund:"+item.ID.String()).Scan(&postings); err != nil {
					t.Fatal(err)
				}
				if applied.RevokedBase == 0 && postings != 0 {
					t.Fatal("zero effect wrote postings", postings)
				}
			}
			var posted, journals, debts int64
			if err := r.pool.QueryRow(ctx, `SELECT posted_microcredits,(SELECT count(*) FROM journal_entries WHERE entry_type='FORCED_REFUND'),(SELECT count(*) FROM receivables) FROM wallets WHERE id=$1`, order.WalletID).Scan(&posted, &journals, &debts); err != nil || posted != tc.credit-total || journals != int64(successful) || debts != 0 {
				t.Fatal("cash receipt or balance mismatch", posted, journals, debts, err)
			}
		})
	}
}

func TestForcedRefundAndChargebackShareCashBudget(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	_, _, transaction, _ := retainedFinanceOrder(t, r, ctx, 1000000, true)
	forced, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "first-forced", 50, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.ApplyReviewedForcedRefund(ctx, forced.ID); err != nil {
		t.Fatal(err)
	}
	charge, err := r.RecordConfirmedChargebackForReview(ctx, transaction, "second-whole-chargeback", nil)
	if err != nil {
		t.Fatal("must retain provider evidence", err)
	}
	if _, err = r.ApplyConfirmedChargeback(ctx, charge.PaymentEvidenceID, charge.ProviderChargebackID); !errors.Is(err, ErrConflict) {
		t.Fatal("whole chargeback reused partial forced cash", err)
	}
	var journals int
	if err = r.pool.QueryRow(ctx, `SELECT count(*) FROM journal_entries WHERE entry_type='CHARGEBACK'`).Scan(&journals); err != nil || journals != 0 {
		t.Fatal("rejected chargeback changed money", journals, err)
	}
}

func TestForcedRefundSourceBudgetConcurrent(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	_, _, transaction, _ := retainedFinanceOrder(t, r, ctx, 1000000, true)
	a, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "concurrent-A", 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "concurrent-B", 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, item := range []ForcedRefundCase{a, b} {
		go func(item ForcedRefundCase) {
			<-start
			_, err := r.ApplyReviewedForcedRefund(ctx, item.ID)
			results <- err
		}(item)
	}
	close(start)
	successes := 0
	for range 2 {
		err := <-results
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrConflict) {
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "40001" {
				t.Fatal("unexpected concurrent error", err)
			}
		}
	}
	if successes != 1 {
		t.Fatal("source gate allowed wrong effect count", successes)
	}
	for _, item := range []ForcedRefundCase{a, b} {
		_, err := r.ApplyReviewedForcedRefund(ctx, item.ID)
		if err != nil && !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
	var journals, debts int
	if err = r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM journal_entries WHERE entry_type='FORCED_REFUND'),(SELECT count(*) FROM receivables)`).Scan(&journals, &debts); err != nil || journals != 1 || debts != 0 {
		t.Fatal("duplicate cash disposition", journals, debts, err)
	}
}

// Successful ordinary refund allocations are not reconstructed from rounded
// cash; mixed legacy allocations remain an explicit manual-review boundary.
func TestForcedRefundNormalRefundEvidenceBoundary(t *testing.T) {
	for _, terminal := range []string{"SUCCESS", "FAILED"} {
		t.Run(terminal, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			c, order, transaction, pack := retainedFinanceOrder(t, r, ctx, 1000000, true)
			c.AllowNormalRefundCreation(true)
			refund, _, err := c.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, uuid.NewString(), "controlled ordinary refund")
			if err != nil {
				t.Fatal(err)
			}
			proof := ProviderRefund{MerchantRefundNo: refund.MerchantRefundNo, AmountFen: 100, Status: terminal, ChannelRefundID: "controlled-normal-proof"}
			if _, _, err = c.finishRefund(ctx, refund, proof, false); err != nil {
				t.Fatal(err)
			}
			forced, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "after-normal-"+terminal, 100, nil)
			if err != nil {
				t.Fatal("provider fact was discarded", err)
			}
			result, err := r.ApplyReviewedForcedRefund(ctx, forced.ID)
			if terminal == "SUCCESS" {
				if !errors.Is(err, ErrConflict) {
					t.Fatal("successful normal refund reused cash", err)
				}
			} else if err != nil || result.RevokedBase != 1000000 {
				t.Fatal("known failed refund blocked unused source", err)
			}
		})
	}
}

func TestForcedRefundUnknownChargebackBudgetFence(t *testing.T) {
	for _, status := range []string{"REVIEW_REQUIRED", "DISMISSED"} {
		t.Run(status, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			_, _, transaction, _ := retainedFinanceOrder(t, r, ctx, 1000000, true)
			charge, err := r.RecordConfirmedChargebackForReview(ctx, transaction, "unknown-chargeback", nil)
			if err != nil {
				t.Fatal(err)
			}
			if status == "DISMISSED" {
				if _, err = r.pool.Exec(ctx, `UPDATE chargeback_cases SET status='DISMISSED',resolved_at=now() WHERE id=$1`, charge.ID); err != nil {
					t.Fatal(err)
				}
			}
			forced, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "new-forced-evidence", 50, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = r.ApplyReviewedForcedRefund(ctx, forced.ID); !errors.Is(err, ErrConflict) {
				t.Fatal("unknown chargeback bypassed", err)
			}
		})
	}
}

func TestForcedRefundCumulativeUnavailableBaseDebt(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	_, order, transaction, pack := retainedFinanceOrder(t, r, ctx, 10, true)
	expired := r.now().Add(-time.Hour)
	for _, statement := range []string{`UPDATE entitlement_packages SET valid_from=$2::timestamptz-interval '30 days',valid_until=$2 WHERE id=$1`, `UPDATE entitlement_components SET credited_at=$2::timestamptz-interval '30 days',expires_at=$2 WHERE package_id=$1`, `UPDATE credit_lots SET expires_at=$2 WHERE id IN(SELECT credit_lot_id FROM entitlement_components WHERE package_id=$1)`} {
		if _, err := r.pool.Exec(ctx, statement, pack, expired); err != nil {
			t.Fatal(err)
		}
	}
	if count, err := r.ExpirePackages(ctx, 1); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	for i, cash := range []int64{33, 33, 34} {
		item, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "expired-cumulative-"+string(rune('A'+i)), cash, nil)
		if err != nil {
			t.Fatal(err)
		}
		result, err := r.ApplyReviewedForcedRefund(ctx, item.ID)
		if err != nil || result.RevokedBase != 0 || result.ReceivableID == nil {
			t.Fatal("unavailable BASE missing exact debt", result, err)
		}
		if i == 0 {
			// Exercise the real settlement helper with controlled cash funding,
			// not an end-user payment or usage-consumption acceptance test.
			tx, err := r.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err = tx.Exec(ctx, `SELECT id FROM wallets WHERE id=$1 FOR UPDATE`, order.WalletID); err != nil {
				t.Fatal(err)
			}
			journalID, now := uuid.New(), r.now()
			if err = insertJournal(ctx, tx, journalID, "RECHARGE", "fixture-debt-repayment", "controlled repayment", now); err != nil {
				t.Fatal(err)
			}
			if settled, err := settleOpenReceivablesWithinTransaction(ctx, tx, order.WalletID, 4, "CASH_RECHARGE_BASE", "fixture-debt-repayment", journalID, now); err != nil || settled != 4 {
				t.Fatal("real debt settlement", settled, err)
			}
			if err = insertPosting(ctx, tx, journalID, receivableAccount, 4, nil, now); err != nil {
				t.Fatal(err)
			}
			if err = insertPosting(ctx, tx, journalID, paymentClearingAccount, -4, nil, now); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	var debt int64
	if err := r.pool.QueryRow(ctx, `SELECT sum(original_microcredits) FROM receivables WHERE entitlement_package_id=$1`, pack).Scan(&debt); err != nil || debt != 10 {
		t.Fatal("cumulative debt rounding repeated", debt, err)
	}
}

func TestForcedRefundLegacyDispositionNeedsReceipt(t *testing.T) {
	for _, applied := range []bool{false, true} {
		t.Run(fmt.Sprint(applied), func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			_, _, transaction, _ := retainedFinanceOrder(t, r, ctx, 10, true)
			previous, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "legacy-disposition", 50, nil)
			if err != nil {
				t.Fatal(err)
			}
			if applied {
				if _, err = r.ApplyReviewedForcedRefund(ctx, previous.ID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = r.pool.Exec(ctx, `UPDATE forced_refund_cases SET status='RESOLVED',resolution_code=NULL,version=version+1,resolved_at=now() WHERE id=$1`, previous.ID); err != nil {
				t.Fatal(err)
			}
			if _, err = r.ApplyReviewedForcedRefund(ctx, previous.ID); !errors.Is(err, ErrConflict) {
				t.Fatal("legacy replay guessed outcome", err)
			}
			next, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "next-fact", 50, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = r.ApplyReviewedForcedRefund(ctx, next.ID); !errors.Is(err, ErrConflict) {
				t.Fatal("unknown old disposition reused budget", err)
			}
		})
	}
}

func TestForcedRefundBudgetExclusionsAreDomainScoped(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	_, order, transaction, pack := retainedFinanceOrder(t, r, ctx, 10, true)
	forced, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "domain-forced", 50, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.ApplyReviewedForcedRefund(ctx, forced.ID); err != nil {
		t.Fatal(err)
	}
	budget, err := readForcedRefundBudget(ctx, r.pool, transaction, pack, order.ID, order.WalletID, uuid.Nil, forced.ID)
	if err != nil || budget.cash != 50 || budget.base != 5 {
		t.Fatal("chargeback exclusion omitted forced domain", budget, err)
	}
	charge, err := r.RecordConfirmedChargebackForReview(ctx, transaction, "domain-chargeback", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = readForcedRefundBudget(ctx, r.pool, transaction, pack, order.ID, order.WalletID, charge.ID, uuid.Nil); !errors.Is(err, ErrConflict) {
		t.Fatal("forced exclusion omitted chargeback domain", err)
	}
}

func TestChargebackConfirmedReplayRequiresReceipt(t *testing.T) {
	for _, applied := range []bool{false, true} {
		t.Run(fmt.Sprint(applied), func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			_, _, transaction, _ := retainedFinanceOrder(t, r, ctx, 10, true)
			charge, err := r.RecordConfirmedChargebackForReview(ctx, transaction, "confirmed-receipt", nil)
			if err != nil {
				t.Fatal(err)
			}
			if applied {
				if _, err = r.ApplyConfirmedChargeback(ctx, charge.PaymentEvidenceID, charge.ProviderChargebackID); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err = r.pool.Exec(ctx, `UPDATE chargeback_cases SET status='CONFIRMED',version=version+1 WHERE id=$1`, charge.ID); err != nil {
					t.Fatal(err)
				}
			}
			_, err = r.ApplyConfirmedChargeback(ctx, charge.PaymentEvidenceID, charge.ProviderChargebackID)
			if applied {
				if err != nil {
					t.Fatal("valid replay failed", err)
				}
			} else if !errors.Is(err, ErrConflict) {
				t.Fatal("unknown CONFIRMED guessed receipt", err)
			}
		})
	}
}
