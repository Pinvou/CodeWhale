package billing

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The original creator, claim and resolution run against owned public-schema
// fixtures. The payment evidence is controlled, not a live provider response.
func closureFinancialAnomaly(t *testing.T, r *PostgresRepository, ctx context.Context, order, transaction uuid.UUID, resolution, financialStatus string) PaymentAnomalyCase {
	t.Helper()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	anomalyType := "PAYMENT_FACT_CONFLICT"
	if resolution == "CONFIRM_PAYMENT" {
		anomalyType = "DUPLICATE_PAYMENT"
	}
	item, err := createPaymentAnomalyWithinTransaction(ctx, tx, anomalyType, "P2", "closure-pending-"+uuid.NewString(), paymentAnomalyReference{PaymentOrderID: &order, PaymentTransactionID: &transaction}, map[string]any{"controlled": true}, r.now())
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if resolution == "" {
		return item
	}
	actor := uuid.New()
	retainedClaimAnomaly(t, r, ctx, item.ID, actor)
	item, err = r.ResolvePaymentAnomaly(ctx, item.ID, actor, uuid.NewString(), resolution, "controlled closure financial resolution", financialStatus)
	if err != nil || item.Status != "RESOLVED" || item.FinancialActionStatus != financialStatus {
		t.Fatal("original financial resolution did not commit", item, err)
	}
	return item
}

func closureFinancialSnapshot(t *testing.T, r *PostgresRepository, ctx context.Context) string {
	t.Helper()
	rows := []string{}
	for _, table := range []string{"wallets", "journal_entries", "ledger_postings", "entitlement_packages", "entitlement_components", "member_quota_grants", "member_quota_grant_audits", "account_closed_grant_reclaim_commands", "usage_holds", "payment_orders", "payment_anomaly_cases"} {
		var raw string
		if err := r.pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY to_jsonb(t)::text),'[]'::jsonb)::text FROM `+pgx.Identifier{table}.Sanitize()+` t`).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, table+"="+raw)
	}
	return strings.Join(rows, "\n")
}

func TestF08ClosureBlocksPendingFinancialAnomalyPostgres(t *testing.T) {
	for _, state := range []struct {
		name, resolution, financialStatus string
		blocked                           bool
	}{
		{"open", "", "NONE", true},
		{"resolved-pending", "ESCALATE_FINANCIAL_REPAIR", "PENDING", true},
		{"resolved-none", "DISMISS", "NONE", false},
		{"resolved-applied", "CONFIRM_PAYMENT", "APPLIED", false},
	} {
		t.Run(state.name, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			_, order, transaction, _ := retainedFinanceOrder(t, r, ctx, 10, true)
			closureFinancialAnomaly(t, r, ctx, order.ID, transaction, state.resolution, state.financialStatus)
			unrelated := uuid.New()
			if _, _, err := r.EnsureWallet(ctx, "PERSONAL", unrelated); err != nil {
				t.Fatal(err)
			}
			before := closureFinancialSnapshot(t, r, ctx)
			assessment, err := r.AccountClosureAssessment(ctx, order.RequesterID)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, blocker := range assessment.Blockers {
				found = found || blocker.Code == "PAYMENT_ANOMALY_OPEN"
			}
			if found != state.blocked {
				t.Fatal("pending financial action was misclassified", state.name, assessment)
			}
			other, err := r.AccountClosureAssessment(ctx, unrelated)
			if err != nil || !other.Ready || len(other.Blockers) != 0 || closureFinancialSnapshot(t, r, ctx) != before {
				t.Fatal("assessment changed facts or blocked an unrelated payer", other, err)
			}
		})
	}
}

func TestF08ReclaimBlocksPendingFinancialAnomalyPostgres(t *testing.T) {
	for _, replay := range []bool{false, true} {
		name := "new-effects-blocked"
		if replay {
			name = "original-receipt-still-replays"
		}
		t.Run(name, func(t *testing.T) {
			r, ctx, wallet, grant, actor := closedGrantWalletFirstFixture(t)
			key, reason := uuid.NewString(), "controlled pending financial reclaim"
			if replay {
				if result, err := r.ReclaimAccountClosedGrant(ctx, wallet.OwnerID, grant.ID, actor, 2, reason, key); err != nil || result.Replayed || result.AmountMicrocredits != 500000 {
					t.Fatal("original reclaim receipt not committed", result, err)
				}
			}
			var order, transaction uuid.UUID
			if err := r.pool.QueryRow(ctx, `SELECT payment_order_id,source_payment_transaction_id FROM entitlement_packages WHERE id=$1`, grant.PackageID).Scan(&order, &transaction); err != nil {
				t.Fatal(err)
			}
			closureFinancialAnomaly(t, r, ctx, order, transaction, "ESCALATE_FINANCIAL_REPAIR", "PENDING")
			before := closureFinancialSnapshot(t, r, ctx)
			result, err := r.ReclaimAccountClosedGrant(ctx, wallet.OwnerID, grant.ID, actor, 2, reason, key)
			if replay {
				if err != nil || !result.Replayed || result.AmountMicrocredits != 500000 {
					t.Fatal("pending case invalidated an original immutable receipt", result, err)
				}
			} else if !errors.Is(err, ErrConflict) {
				t.Fatal("unexecuted financial repair allowed new reclaim", result, err)
			}
			if closureFinancialSnapshot(t, r, ctx) != before {
				t.Fatal("blocked reclaim or receipt replay changed money or evidence")
			}
		})
	}
}
