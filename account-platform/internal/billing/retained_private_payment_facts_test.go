package billing

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// This test covers the owner merge with persisted provider facts, not Account
// approval delivery, production activation, or a live provider connection.
func TestRetainedPaymentRepairProviderFacts(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	activationVersion := paymentFixtureActivation(t, ctx, r.pool)("ACTIVE")
	provider := &paymentRepairStubProvider{}
	p, err := NewPaymentCoordinatorWithProvider(r, provider)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"correct", "missing-customer", "foreign-customer", "missing-sku", "foreign-sku", "customer-null", "customer-number"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Now().UTC()
			orderID, walletID, owner, evidenceID, transactionID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
			product, err := json.Marshal(Product{ProductID: "PAYMENT_REPAIR", AmountFen: 100, CreditMicrocredits: 100, BasePoints: 80, BonusPoints: 20, Currency: "CNY", PlanName: "Repair"})
			if err != nil {
				t.Fatal(err)
			}
			tx, err := r.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			for _, q := range []struct {
				sql  string
				args []any
			}{
				{`INSERT INTO wallets(id,owner_type,owner_id,status,posted_microcredits,version,created_at,updated_at) VALUES($1,'PERSONAL',$2,'ACTIVE',0,1,$3,$3)`, []any{walletID, owner, now}},
				{`INSERT INTO ledger_accounts(id,wallet_id,code,account_type,created_at) VALUES($1,$2,$3,'WALLET_LIABILITY',$4)`, []any{uuid.New(), walletID, "wallet:" + walletID.String(), now}},
				{`INSERT INTO payment_orders(id,merchant_order_no,requester_id,payer_user_id,wallet_id,product_id,product_snapshot,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,provider_payable_until,late_review_boundary_at,final_archive_eligible_at,created_at,updated_at,provider_sku,provider_id,provider_version) VALUES($1,$2,$3,$3,$4,'PAYMENT_REPAIR',$5,'ALIPAY',100,'CNY',100,'MANUAL_REVIEW','MANUAL_REVIEW',$6,$6,$7,$8,$9,$9,'sku-repair','test-provider','v1')`, []any{orderID, "repair-" + orderID.String(), owner, walletID, product, now.Add(time.Hour), now.Add(72 * time.Hour), now.Add(8 * 24 * time.Hour), now}},
				{`INSERT INTO payment_transactions(id,payment_order_id,provider_id,provider_version,channel,provider_transaction_id,status,fulfillment_status,amount_fen,currency,source,provider_status,provider_paid_at,verified_at,created_at,updated_at) VALUES($1,$2,'test-provider','v1','ALIPAY',$3,'REVIEW_REQUIRED','REVIEW_REQUIRED',100,'CNY','QUERY','PAY_SUCCESS',$4,$4,$4,$4)`, []any{transactionID, orderID, "trade-" + evidenceID.String(), now}},
			} {
				if _, err = tx.Exec(ctx, q.sql, q.args...); err != nil {
					t.Fatal(err)
				}
			}
			order, err := getPaymentOrderByID(ctx, tx, orderID, false)
			if err != nil {
				t.Fatal(err)
			}
			trade := "trade-" + evidenceID.String()
			fact := ProviderOrder{MerchantOrderNo: order.MerchantOrderNo, Status: "SUCCESS", Channel: "ALIPAY", AmountFen: 100, Currency: "CNY", ProviderStatus: "PAY_SUCCESS", ChannelTransactionID: &trade, PaidAt: &now, ExternalCustomerKey: owner.String(), ExternalSKU: "sku-repair"}
			raw, err := json.Marshal(fact)
			if err != nil {
				t.Fatal(err)
			}
			var snapshot map[string]any
			if err = json.Unmarshal(raw, &snapshot); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "missing-customer":
				delete(snapshot, "external_customer_key")
			case "foreign-customer":
				snapshot["external_customer_key"] = uuid.NewString()
			case "missing-sku":
				delete(snapshot, "external_sku")
			case "foreign-sku":
				snapshot["external_sku"] = "foreign-sku"
			case "customer-null":
				snapshot["external_customer_key"] = nil
			case "customer-number":
				snapshot["external_customer_key"] = 12
			}
			raw, err = json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			evidenceID, err = p.insertPaymentEvidence(ctx, tx, order, &transactionID, "QUERY", "query:"+evidenceID.String(), "RECORDED", fact, raw, now)
			if err != nil {
				t.Fatal("actual immutable evidence producer", err)
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			counts := func() string {
				var s string
				if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_array((SELECT count(*) FROM entitlement_packages),(SELECT count(*) FROM journal_entries),(SELECT count(*) FROM ledger_postings),(SELECT count(*) FROM payment_order_repairs),(SELECT count(*) FROM outbox_events),(SELECT COALESCE(sum(posted_microcredits),0) FROM wallets))::text`).Scan(&s); err != nil {
					t.Fatal(err)
				}
				return s
			}
			before := counts()
			cmd := PaymentFinancialRepairCommandInput{PaymentOrderID: orderID, PaymentEvidenceID: evidenceID}
			plan, err := paymentFinancialRepairPlanFrom(ctx, r.pool, cmd, 1, 1, activationVersion)
			if kind != "correct" {
				if !errors.Is(err, ErrApprovalSnapshotMismatch) {
					t.Errorf("invalid durable provider %s accepted as plan: %v", kind, err)
				}
				if counts() != before {
					t.Error("rejected plan mutated actual owner facts")
				}
				return
			}
			if err != nil {
				t.Fatal("legitimate immutable provider plan", err)
			}
			if plan.Provider.ExternalCustomerKey != owner.String() || plan.Provider.ExternalSKU != "sku-repair" {
				t.Errorf("plan did not preserve durable customer/SKU: %q/%q", plan.Provider.ExternalCustomerKey, plan.Provider.ExternalSKU)
			}
			input := PaymentFinancialRepairExecutionInput{ApprovalRequestID: uuid.New(), AuthorizationEventID: uuid.New(), ApprovalEpoch: 2, ApproverUserID: uuid.New(), RequesterUserID: uuid.New(), ExecutionKey: "payment-repair-" + uuid.NewString(), PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: activationVersion, Command: cmd, Frozen: PaymentFinancialRepairFrozenFacts{OrderUpdatedAt: plan.OrderUpdatedAt, AmountFen: plan.AmountFen, CreditMicrocredits: plan.CreditMicrocredits, ProviderTransactionID: plan.ProviderTransactionID, WalletID: plan.WalletID, WalletVersion: plan.WalletVersion, AppliedToReceivables: plan.AppliedToReceivables, FactsDigest: plan.FactsDigest}}
			result, err := p.ApplyPaymentFinancialRepair(ctx, input)
			if err != nil {
				t.Fatal("actual strict matcher/merge/credit owner apply", err)
			}
			var actor, requester uuid.UUID
			var balance int64
			var postings int
			var sum int64
			if err = r.pool.QueryRow(ctx, `SELECT actor_id,requester_user_id FROM payment_order_repairs WHERE id=$1`, result.RepairID).Scan(&actor, &requester); err != nil || actor != input.ApproverUserID || requester != input.RequesterUserID {
				t.Error("actual distinct approval audit identities", err)
			}
			if err = r.pool.QueryRow(ctx, `SELECT posted_microcredits FROM wallets WHERE id=$1`, walletID).Scan(&balance); err != nil || balance != 100 {
				t.Error("actual wallet credit", balance, err)
			}
			if err = r.pool.QueryRow(ctx, `SELECT count(*),COALESCE(sum(delta_microcredits),0) FROM ledger_postings WHERE journal_entry_id=$1`, result.JournalEntryID).Scan(&postings, &sum); err != nil || postings != 3 || sum != 0 {
				t.Error("actual BASE/BONUS balanced postings", postings, sum, err)
			}
			after := counts()
			again, err := p.ApplyPaymentFinancialRepair(ctx, input)
			if err != nil || again != result || counts() != after {
				t.Error("actual result-first idempotent replay", err)
			}
		})
	}
	if provider.calls != 0 {
		t.Errorf("repair used external provider calls=%d", provider.calls)
	}
}
