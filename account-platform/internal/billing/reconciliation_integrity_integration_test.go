package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"strings"
	"testing"
)

func reconciliationAuditPayment(t *testing.T, r *PostgresRepository, ctx context.Context, order PaymentOrder, transaction uuid.UUID) ReconciliationInternalFact {
	t.Helper()
	snapshot, _ := json.Marshal(map[string]any{"payment_transaction": map[string]any{"id": transaction}})
	facts := []ReconciliationInternalFact{{EntryType: "PAYMENT", PaymentOrderID: order.ID, InternalSnapshot: snapshot}}
	if err := auditFourWayReconciliationFacts(ctx, r.pool, facts); err != nil {
		t.Fatal(err)
	}
	return facts[0]
}

// Faults are installed only in the fixture's isolated UUID database; the audit
// itself uses the actual Billing runtime role and migrated storage.
func TestF03FourWayPostingAndPackageIntegrity(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	for _, scenario := range []struct{ name, sql, want string }{
		{"normal", "", ""},
		{"empty-journal", `ALTER TABLE ledger_postings DISABLE TRIGGER ledger_postings_immutable; DELETE FROM ledger_postings WHERE journal_entry_id IN (SELECT id FROM journal_entries WHERE business_reference='payment-transaction:TRANSACTION'); ALTER TABLE ledger_postings ENABLE TRIGGER ledger_postings_immutable;`, "PAYMENT_LEDGER_INVALID"},
		{"wrong-wallet", `ALTER TABLE ledger_postings DISABLE TRIGGER ledger_postings_immutable; UPDATE ledger_postings SET ledger_account_id='OTHER_ACCOUNT' WHERE credit_lot_id IS NOT NULL AND journal_entry_id IN (SELECT id FROM journal_entries WHERE business_reference='payment-transaction:TRANSACTION'); ALTER TABLE ledger_postings ENABLE TRIGGER ledger_postings_immutable;`, "PAYMENT_LEDGER_INVALID"},
		{"balanced-wrong-amount", `SET ROLE billing_schema_owner; ALTER TABLE ledger_postings DISABLE TRIGGER ledger_postings_immutable; UPDATE ledger_postings SET delta_microcredits=delta_microcredits*2 WHERE journal_entry_id IN (SELECT id FROM journal_entries WHERE business_reference='payment-transaction:TRANSACTION'); ALTER TABLE ledger_postings ENABLE TRIGGER ledger_postings_immutable;`, "PAYMENT_LEDGER_INVALID"},
		{"wrong-clearing", `SET ROLE billing_schema_owner; ALTER TABLE ledger_postings DISABLE TRIGGER ledger_postings_immutable; UPDATE ledger_postings SET ledger_account_id='00000000-0000-7000-8000-000000000003' WHERE ledger_account_id='00000000-0000-7000-8000-000000000001' AND journal_entry_id IN (SELECT id FROM journal_entries WHERE business_reference='payment-transaction:TRANSACTION'); ALTER TABLE ledger_postings ENABLE TRIGGER ledger_postings_immutable;`, "PAYMENT_LEDGER_INVALID"},
		{"wrong-lot", `SET ROLE billing_schema_owner; ALTER TABLE ledger_postings DISABLE TRIGGER ledger_postings_immutable; UPDATE ledger_postings SET credit_lot_id='OTHER_LOT' WHERE credit_lot_id IS NOT NULL AND journal_entry_id IN (SELECT id FROM journal_entries WHERE business_reference='payment-transaction:TRANSACTION'); ALTER TABLE ledger_postings ENABLE TRIGGER ledger_postings_immutable;`, "PAYMENT_LEDGER_INVALID"},
		{"missing-source-package", `UPDATE entitlement_packages SET source_payment_transaction_id=NULL,payment_order_id=NULL,source_type='SYSTEM' WHERE source_payment_transaction_id='TRANSACTION';`, "PAYMENT_PACKAGE_INVALID"},
		{"consumed-source", `UPDATE entitlement_components SET available_microcredits=0,used_microcredits=original_microcredits,status='CONSUMED' WHERE package_id IN (SELECT id FROM entitlement_packages WHERE source_payment_transaction_id='TRANSACTION'); UPDATE entitlement_packages SET points_remaining=0,points_used=points_total,status='CONSUMED' WHERE source_payment_transaction_id='TRANSACTION';`, ""},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			_, order, transaction, _ := retainedFinanceOrder(t, r, ctx, 1000000, true)
			otherWallet, _, err := r.EnsureWallet(ctx, "PERSONAL", uuid.New())
			if err != nil {
				t.Fatal(err)
			}
			tx, err := r.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			otherAccount, err := walletLedgerAccount(ctx, tx, otherWallet.ID)
			if err != nil {
				tx.Rollback(ctx)
				t.Fatal(err)
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if scenario.sql != "" {
				_, _, otherTransaction, _ := retainedFinanceOrder(t, r, ctx, 1000000, true)
				var otherLot uuid.UUID
				if err = r.pool.QueryRow(ctx, `SELECT credit_lot_id FROM entitlement_packages WHERE source_payment_transaction_id=$1`, otherTransaction).Scan(&otherLot); err != nil {
					t.Fatal(err)
				}
				statement := strings.ReplaceAll(scenario.sql, "TRANSACTION", transaction.String())
				statement = strings.ReplaceAll(statement, "OTHER_ACCOUNT", otherAccount.String())
				statement = strings.ReplaceAll(statement, "OTHER_LOT", otherLot.String())
				statement = strings.ReplaceAll(statement, "ALTER TABLE ledger_postings ENABLE", "SET CONSTRAINTS ALL IMMEDIATE; ALTER TABLE ledger_postings ENABLE")
				if _, err := retainedFinanceOwnerExec(t, r, ctx, "BEGIN; "+statement+" COMMIT;"); err != nil {
					t.Fatal(err)
				}
			}
			fact := reconciliationAuditPayment(t, r, ctx, order, transaction)
			if fact.IntegrityCaseType != scenario.want || !fact.LedgerPresent || fact.LedgerValid != (scenario.want == "") {
				t.Fatalf("unexpected audit result: %s valid=%v", fact.IntegrityCaseType, fact.LedgerValid)
			}
		})
	}
}

func TestF03FourWayRechargePaysReceivables(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	for _, debt := range []int64{400000, 1000000} {
		t.Run(fmt.Sprint(debt), func(t *testing.T) {
			c, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
			tx, err := r.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err = createReceivableWithinTransaction(ctx, tx, order.WalletID, "USAGE_OVERAGE", "audit-debt:"+uuid.NewString(), debt, nil, nil, nil, nil, r.now()); err != nil {
				t.Fatal(err)
			}
			provider := c.provider.(*retainedFinancialProvider)
			order, _, err = c.mergeSuccessfulPaymentWithinTransaction(ctx, tx, order, "QUERY", "audit-repay:"+uuid.NewString(), []byte(`{"controlled":true}`), provider.result, false, r.now())
			if err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			var transaction uuid.UUID
			if err = r.pool.QueryRow(ctx, `SELECT id FROM payment_transactions WHERE payment_order_id=$1`, order.ID).Scan(&transaction); err != nil {
				t.Fatal(err)
			}
			fact := reconciliationAuditPayment(t, r, ctx, order, transaction)
			if !fact.LedgerValid || fact.IntegrityCaseType != "" {
				t.Fatalf("legitimate receivable settlement rejected: %s", fact.IntegrityCaseType)
			}
			wallet, err := r.GetWallet(ctx, order.WalletID)
			if err != nil || wallet.PostedMicrocredits != 1000000-debt {
				t.Fatal("fixture did not exercise net wallet credit", err)
			}
		})
	}
}

func TestF03FourWayRefundAndChargebackSources(t *testing.T) {
	for _, kind := range []string{"refund", "forced", "chargeback"} {
		t.Run(kind, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			c, order, transaction, pack := retainedFinanceOrder(t, r, ctx, 1000000, true)
			switch kind {
			case "refund":
				c.AllowNormalRefundCreation(true)
				refund, _, err := c.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, uuid.NewString(), "controlled audit refund")
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err = c.finishRefund(ctx, refund, ProviderRefund{MerchantRefundNo: refund.MerchantRefundNo, AmountFen: refund.AmountFen, Status: "SUCCESS", ChannelRefundID: "audit-success"}, false); err != nil {
					t.Fatal(err)
				}
			case "forced":
				forced, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "audit-forced", order.AmountFen, json.RawMessage(`{"controlled":true}`))
				if err != nil {
					t.Fatal(err)
				}
				actor, key := uuid.New(), uuid.NewString()
				retainedClaimAnomaly(t, r, ctx, *forced.ManualReviewCaseID, actor)
				if _, _, err = r.PreparePaymentAnomalyResolution(ctx, *forced.ManualReviewCaseID, actor, key, "CONFIRM_FORCED_REFUND", "controlled audit"); err != nil {
					t.Fatal(err)
				}
				if _, err = r.ApplyReviewedForcedRefund(ctx, forced.ID); err != nil {
					t.Fatal(err)
				}
				if _, err = retainedResolveClaimedAnomaly(t, r, ctx, *forced.ManualReviewCaseID, actor, key, "CONFIRM_FORCED_REFUND", "controlled audit", "APPLIED"); err != nil {
					t.Fatal(err)
				}
			case "chargeback":
				chargeback, err := r.RecordConfirmedChargebackForReview(ctx, transaction, "audit-chargeback", json.RawMessage(`{"controlled":true}`))
				if err != nil {
					t.Fatal(err)
				}
				actor, key := uuid.New(), uuid.NewString()
				retainedClaimAnomaly(t, r, ctx, *chargeback.ManualReviewCaseID, actor)
				if _, _, err = r.PreparePaymentAnomalyResolution(ctx, *chargeback.ManualReviewCaseID, actor, key, "CONFIRM_CHARGEBACK", "controlled audit"); err != nil {
					t.Fatal(err)
				}
				if _, err = r.ApplyConfirmedChargeback(ctx, chargeback.PaymentEvidenceID, "audit-chargeback"); err != nil {
					t.Fatal(err)
				}
				if _, err = retainedResolveClaimedAnomaly(t, r, ctx, *chargeback.ManualReviewCaseID, actor, key, "CONFIRM_CHARGEBACK", "controlled audit", "APPLIED"); err != nil {
					t.Fatal(err)
				}
			}
			start, end := reconciliationBusinessDateRange(normalizedReconciliationBusinessDate(r.now()))
			facts, err := c.listFourWayReconciliationFacts(ctx, r.pool, order.Channel, start, end)
			if err != nil {
				t.Fatal(err)
			}
			if len(facts) != 2 {
				t.Fatalf("expected payment and reversal facts, got %d", len(facts))
			}
			if err = auditFourWayReconciliationFacts(ctx, r.pool, facts); err != nil {
				t.Fatal(err)
			}
			for _, fact := range facts {
				if !fact.LedgerValid || fact.IntegrityCaseType != "" {
					t.Fatalf("valid %s source rejected: %s", fact.EntryType, fact.IntegrityCaseType)
				}
			}
		})
	}
}

func TestF03FourWayRefundCashCannotRevokeWrongFraction(t *testing.T) {
	for _, kind := range []string{"whole-refund", "forced-full", "forced-partial"} {
		t.Run(kind, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			c, order, transaction, pack := retainedFinanceOrder(t, r, ctx, 1000000, true)
			var source uuid.UUID
			table := "refunds"
			if kind == "whole-refund" {
				c.AllowNormalRefundCreation(true)
				refund, _, err := c.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, uuid.NewString(), "audit whole")
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err = c.finishRefund(ctx, refund, ProviderRefund{MerchantRefundNo: refund.MerchantRefundNo, AmountFen: 100, Status: "SUCCESS", ChannelRefundID: "audit-fraction"}, false); err != nil {
					t.Fatal(err)
				}
				source = refund.ID
			} else {
				amount := int64(100)
				if kind == "forced-partial" {
					amount = 50
				}
				forced, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "audit-fraction", amount, json.RawMessage(`{"controlled":true}`))
				if err != nil {
					t.Fatal(err)
				}
				actor, key := uuid.New(), uuid.NewString()
				retainedClaimAnomaly(t, r, ctx, *forced.ManualReviewCaseID, actor)
				if _, _, err = r.PreparePaymentAnomalyResolution(ctx, *forced.ManualReviewCaseID, actor, key, "CONFIRM_FORCED_REFUND", "audit fraction"); err != nil {
					t.Fatal(err)
				}
				if _, err = r.ApplyReviewedForcedRefund(ctx, forced.ID); err != nil {
					t.Fatal(err)
				}
				if _, err = retainedResolveClaimedAnomaly(t, r, ctx, *forced.ManualReviewCaseID, actor, key, "CONFIRM_FORCED_REFUND", "audit fraction", "APPLIED"); err != nil {
					t.Fatal(err)
				}
				table = "forced_refund_cases"
				source = forced.ID
			}
			check := func(wantValid bool) {
				t.Helper()
				start, end := reconciliationBusinessDateRange(normalizedReconciliationBusinessDate(r.now()))
				facts, err := c.listFourWayReconciliationFacts(ctx, r.pool, order.Channel, start, end)
				if err != nil {
					t.Fatal(err)
				}
				if err = auditFourWayReconciliationFacts(ctx, r.pool, facts); err != nil {
					t.Fatal(err)
				}
				found := false
				for _, fact := range facts {
					if fact.EntryType == "REFUND" {
						found = true
						if fact.LedgerValid != wantValid || (!wantValid && fact.IntegrityCaseType != "REFUND_LEDGER_INVALID") {
							t.Fatalf("cash fraction audit valid=%v case=%s", fact.LedgerValid, fact.IntegrityCaseType)
						}
					}
				}
				if !found {
					t.Fatal("actual refund fact absent")
				}
			}
			check(true)
			if kind == "forced-partial" {
				statement := fmt.Sprintf("BEGIN; ALTER TABLE forced_refund_cases DISABLE TRIGGER USER; ALTER TABLE ledger_postings DISABLE TRIGGER ledger_postings_immutable; UPDATE forced_refund_cases SET revoked_base_microcredits=revoked_base_microcredits*2 WHERE id='%s'; UPDATE ledger_postings SET delta_microcredits=delta_microcredits*2 WHERE journal_entry_id IN (SELECT id FROM journal_entries WHERE business_reference='forced-refund:%s'); SET CONSTRAINTS ALL IMMEDIATE; ALTER TABLE forced_refund_cases ENABLE TRIGGER USER; ALTER TABLE ledger_postings ENABLE TRIGGER ledger_postings_immutable; COMMIT;", source, source)
				if _, err := retainedFinanceOwnerExec(t, r, ctx, statement); err != nil {
					t.Fatal(err)
				}
				check(false)
				return
			}
			// Keep posted microcredits unchanged while corrupting cash to half. A
			// balanced journal and matching case quantities must not hide this fault.
			statement := fmt.Sprintf("BEGIN; ALTER TABLE %s DISABLE TRIGGER USER; UPDATE %s SET amount_fen=50 WHERE id='%s'; SET CONSTRAINTS ALL IMMEDIATE; ALTER TABLE %s ENABLE TRIGGER USER; COMMIT;", table, table, source, table)
			if _, err := retainedFinanceOwnerExec(t, r, ctx, statement); err != nil {
				t.Fatal(err)
			}
			check(false)
		})
	}
}

func TestF03FourWayPartialForcedRefundKeepsConsumedBonusOutOfDebt(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	c, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
	product, _ := json.Marshal(Product{ProductID: order.ProductID, PlanName: "controlled mixed source", AmountFen: 100, CreditMicrocredits: 1000000, BasePoints: 3, BonusPoints: 1, ValidityDays: 30, Currency: "CNY"})
	if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET product_snapshot=$2 WHERE id=$1`, order.ID, product); err != nil {
		t.Fatal(err)
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	order, _, err = c.mergeSuccessfulPaymentWithinTransaction(ctx, tx, order, "QUERY", "audit-mixed:"+uuid.NewString(), []byte(`{"controlled":true}`), c.provider.(*retainedFinancialProvider).result, false, r.now())
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var transaction, pack uuid.UUID
	if err = r.pool.QueryRow(ctx, `SELECT t.id,p.id FROM payment_transactions t JOIN entitlement_packages p ON p.source_payment_transaction_id=t.id WHERE t.payment_order_id=$1`, order.ID).Scan(&transaction, &pack); err != nil {
		t.Fatal(err)
	}
	// Install a coherent historical consumption fixture with actual balanced
	// usage postings. This does not impersonate external model/token usage.
	journal := uuid.New()
	statement := fmt.Sprintf(`BEGIN;
 INSERT INTO journal_entries(id,entry_type,business_reference,description,created_at) VALUES('%s','USAGE','audit-consumption:%s','controlled consumption',now());
 INSERT INTO ledger_postings(id,journal_entry_id,ledger_account_id,delta_microcredits,credit_lot_id,created_at)
 SELECT gen_random_uuid(),'%s',a.id,-CASE WHEN c.component_type='BASE' THEN 600000 ELSE 200000 END,c.credit_lot_id,now() FROM entitlement_components c JOIN ledger_accounts a ON a.wallet_id='%s' AND a.account_type='WALLET_LIABILITY' WHERE c.package_id='%s';
 INSERT INTO ledger_postings(id,journal_entry_id,ledger_account_id,delta_microcredits,created_at) VALUES(gen_random_uuid(),'%s','00000000-0000-7000-8000-000000000002',800000,now());
 UPDATE credit_lots l SET remaining_microcredits=CASE WHEN c.component_type='BASE' THEN 150000 ELSE 50000 END FROM entitlement_components c WHERE c.credit_lot_id=l.id AND c.package_id='%s';
 UPDATE entitlement_components SET available_microcredits=CASE WHEN component_type='BASE' THEN 150000 ELSE 50000 END,used_microcredits=CASE WHEN component_type='BASE' THEN 600000 ELSE 200000 END WHERE package_id='%s';
 UPDATE entitlement_packages SET points_remaining=200000,points_used=800000 WHERE id='%s';
 UPDATE wallets SET posted_microcredits=200000 WHERE id='%s'; COMMIT;`, journal, journal, journal, order.WalletID, pack, journal, pack, pack, pack, order.WalletID)
	if _, err = retainedFinanceOwnerExec(t, r, ctx, statement); err != nil {
		t.Fatal(err)
	}
	forced, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "audit-mixed-partial", 50, json.RawMessage(`{"controlled":true}`))
	if err != nil {
		t.Fatal(err)
	}
	actor, key := uuid.New(), uuid.NewString()
	retainedClaimAnomaly(t, r, ctx, *forced.ManualReviewCaseID, actor)
	if _, _, err = r.PreparePaymentAnomalyResolution(ctx, *forced.ManualReviewCaseID, actor, key, "CONFIRM_FORCED_REFUND", "audit mixed fraction"); err != nil {
		t.Fatal(err)
	}
	applied, err := r.ApplyReviewedForcedRefund(ctx, forced.ID)
	if err != nil {
		t.Fatal(err)
	}
	if applied.RevokedBase != 150000 || applied.RevokedBonus != 50000 {
		t.Fatal("fixture did not exercise consumed component shortfalls")
	}
	if _, err = retainedResolveClaimedAnomaly(t, r, ctx, *forced.ManualReviewCaseID, actor, key, "CONFIRM_FORCED_REFUND", "audit mixed fraction", "APPLIED"); err != nil {
		t.Fatal(err)
	}
	wallet, err := r.GetWallet(ctx, order.WalletID)
	if err != nil || wallet.ReceivableMicrocredits != 225000 || wallet.PostedMicrocredits != 0 {
		t.Fatal("BASE target debt or BONUS no-debt contract changed", err)
	}
	start, end := reconciliationBusinessDateRange(normalizedReconciliationBusinessDate(r.now()))
	facts, err := c.listFourWayReconciliationFacts(ctx, r.pool, order.Channel, start, end)
	if err != nil {
		t.Fatal(err)
	}
	if err = auditFourWayReconciliationFacts(ctx, r.pool, facts); err != nil {
		t.Fatal(err)
	}
	if len(facts) != 2 {
		t.Fatal("missing actual reversal fact")
	}
	for _, fact := range facts {
		if !fact.LedgerValid {
			t.Fatalf("valid mixed fraction rejected: %s", fact.IntegrityCaseType)
		}
	}
}
