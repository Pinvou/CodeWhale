package billing

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

type reconciliationFixtureProvider struct {
	retainedFinancialProvider
	rows       []ProviderBillEntry
	queryCount int
}

func (p *reconciliationFixtureProvider) Capabilities() PaymentCapabilities {
	c := p.retainedFinancialProvider.Capabilities()
	c.BillSupported, c.BillFileImportSupported = true, true
	return c
}
func (p *reconciliationFixtureProvider) Bill(context.Context, string, time.Time) ([]ProviderBillEntry, error) {
	return p.rows, nil
}
func (p *reconciliationFixtureProvider) Get(ctx context.Context, merchant string) (ProviderOrder, error) {
	p.queryCount++
	return p.retainedFinancialProvider.Get(ctx, merchant)
}

// Uses the current full F02 migration wrapper, non-superuser Billing actors
// and one UUID database. Supplier query/bill values are controlled, not a live
// vendor acceptance. Wallet posting and reconciliation storage are real PG.
func TestF03ReconciliationAuthorityAndReceipts(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	_, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
	const sku = "controlled-frozen-source-sku"
	if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET provider_sku=$2 WHERE id=$1`, order.ID, sku); err != nil {
		t.Fatal(err)
	}
	order.ProviderSKU = sku
	now, transactionID := r.now(), uuid.NewString()
	row := ProviderBillEntry{EntryType: "PAYMENT", ProviderReferenceID: transactionID, ChannelTransactionID: transactionID, MerchantOrderNo: order.MerchantOrderNo, Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, Status: "SUCCESS", PaidAt: now}
	provider := &reconciliationFixtureProvider{retainedFinancialProvider: retainedFinancialProvider{result: ProviderOrder{MerchantOrderNo: order.MerchantOrderNo, Status: "SUCCESS", Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, ChannelTransactionID: &transactionID, PaidAt: &now, ExternalCustomerKey: uuid.NewString(), ExternalSKU: sku}}, rows: []ProviderBillEntry{row}}
	c, err := NewPaymentCoordinatorWithProvider(r, provider, "test")
	if err != nil {
		t.Fatal(err)
	}
	date := normalizedReconciliationBusinessDate(now)
	actor := uuid.New()
	t.Run("bill-cannot-synthesize-customer-authority", func(t *testing.T) {
		run, err := c.RunAdminReconciliation(ctx, actor, order.Channel, date)
		if err != nil || run.CaseCount != 1 {
			t.Fatal("unconfirmed bill must retain missing-payment case", err)
		}
		actual, err := r.GetWallet(ctx, order.WalletID)
		if err != nil || actual.PostedMicrocredits != 0 {
			t.Fatal("bill credited an unconfirmed customer", err)
		}
		var status string
		var transactions int
		if err := r.pool.QueryRow(ctx, `SELECT status,(SELECT count(*) FROM payment_transactions WHERE payment_order_id=$1) FROM payment_orders WHERE id=$1`, order.ID).Scan(&status, &transactions); err != nil || status != "PENDING" || transactions != 0 {
			t.Fatal("bill changed payment facts without authority", err)
		}
		var evidence int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM payment_evidence WHERE payment_order_id=$1 AND evidence_type='BILL' AND merge_result='REVIEW_REQUIRED'`, order.ID).Scan(&evidence); err != nil || evidence != 1 {
			t.Fatal("real unconfirmed bill evidence missing", err)
		}
	})
	t.Run("matching-query-credits-once-and-import-replays", func(t *testing.T) {
		provider.result.ExternalCustomerKey = order.RequesterID.String()
		run, err := c.RunAdminReconciliation(ctx, actor, order.Channel, date)
		if err != nil || run.CaseCount != 0 {
			t.Fatal("confirmed bill did not reconcile actual payment", err)
		}
		first, err := c.ImportProviderBill(ctx, actor, order.Channel, date, []ProviderBillEntry{row})
		if err != nil {
			t.Fatal(err)
		}
		second, err := c.ImportProviderBill(ctx, actor, order.Channel, date, []ProviderBillEntry{row})
		firstJSON, _ := json.Marshal(first)
		secondJSON, _ := json.Marshal(second)
		if err != nil || string(firstJSON) != string(secondJSON) {
			t.Logf("controlled receipt first=%s replay=%s", firstJSON, secondJSON)
			t.Fatal("identical import did not replay its receipt", err)
		}
		wallet, err := r.GetWallet(ctx, order.WalletID)
		var journals, transactions int
		if err != nil || wallet.PostedMicrocredits != 1000000 {
			t.Fatal("reconciliation changed confirmed funding", err)
		}
		if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM journal_entries WHERE entry_type='RECHARGE'),(SELECT count(*) FROM payment_transactions WHERE payment_order_id=$1)`, order.ID).Scan(&journals, &transactions); err != nil || journals != 1 || transactions != 1 {
			t.Fatal("duplicated payment effect", err)
		}
		var authority int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM payment_evidence WHERE payment_order_id=$1 AND evidence_type='QUERY' AND payload_snapshot->>'external_customer_key'=$2 AND payload_snapshot->>'external_sku'=$3`, order.ID, order.RequesterID.String(), sku).Scan(&authority); err != nil || authority != 1 {
			t.Fatal("actual query authority was not durably evidenced", err)
		}
	})
	t.Run("unsuccessful-import-cannot-mask-confirmed-payment", func(t *testing.T) {
		failed := row
		failed.Status = "FAILED"
		if _, err := c.ImportProviderBill(ctx, actor, order.Channel, date, []ProviderBillEntry{failed}); !errors.Is(err, ErrConflict) {
			t.Fatal("failed provider row accepted as paid fact", err)
		}
	})
	t.Run("scheduled-day-is-idempotent", func(t *testing.T) {
		first, err := c.RunScheduledReconciliation(ctx, order.Channel, date)
		if err != nil {
			t.Fatal(err)
		}
		second, err := c.RunScheduledReconciliation(ctx, order.Channel, date)
		firstJSON, _ := json.Marshal(first)
		secondJSON, _ := json.Marshal(second)
		if err != nil || string(firstJSON) != string(secondJSON) {
			t.Logf("controlled receipt first=%s replay=%s", firstJSON, secondJSON)
			t.Fatal("scheduled day reran its durable batch", err)
		}
	})
	t.Run("case-command-replays-original-receipt-and-actor", func(t *testing.T) {
		// An extra provider refund has a distinct reference and no internal refund;
		// the production comparison creates the actual review case.
		refund := row
		refund.EntryType = "REFUND"
		refund.ProviderReferenceID = "refund-" + uuid.NewString()
		run, err := c.ImportProviderBill(ctx, actor, order.Channel, date, []ProviderBillEntry{row, refund})
		if err != nil || run.CaseCount != 1 {
			t.Fatal("refund difference missing", err)
		}
		items, err := r.AdminReconciliationCases(ctx, run.ID, 20)
		if err != nil || len(items) != 1 || items[0].CaseType != "PROVIDER_REFUND_INTERNAL_MISSING" {
			t.Fatal("refund case not stored", err)
		}
		command := ReconciliationCaseCommand{Action: "CLAIM", ActorID: actor, Reason: "controlled case claim reason", IdempotencyKey: uuid.NewString()}
		first, err := r.UpdateReconciliationCase(ctx, items[0].ID, command)
		if err != nil {
			t.Fatal(err)
		}
		resolve := ReconciliationCaseCommand{Action: "IGNORE", ActorID: actor, Reason: "controlled case resolution reason", ReviewResult: "actual investigation complete", IdempotencyKey: uuid.NewString()}
		if _, err := r.UpdateReconciliationCase(ctx, first.ID, resolve); err != nil {
			t.Fatal(err)
		}
		replay, err := r.UpdateReconciliationCase(ctx, first.ID, command)
		a, _ := json.Marshal(first)
		b, _ := json.Marshal(replay)
		if err != nil || string(a) != string(b) {
			t.Fatal("old invocation returned later case state", err)
		}
		command.ActorID = uuid.New()
		if _, err := r.UpdateReconciliationCase(ctx, first.ID, command); !errors.Is(err, ErrConflict) {
			t.Fatal("different actor replayed another actor receipt", err)
		}
		actual, err := r.AdminReconciliationCases(ctx, run.ID, 20)
		if err != nil || actual[0].Status != "IGNORED" {
			t.Fatal("replay changed actual resolved case", err)
		}
	})
}

func TestF03StatusSpelling(t *testing.T) {
	for _, status := range []string{" success ", " paid ", " completed ", "lowercase-channel"} {
		t.Run(status, func(t *testing.T) {
			repo, ctx := retainedFinanceRepository(t)
			_, order, _, _ := retainedFinanceOrder(t, repo, ctx, 1000000, false)
			sku := "controlled-frozen-source-sku"
			if _, err := repo.pool.Exec(ctx, `UPDATE payment_orders SET provider_sku=$2 WHERE id=$1`, order.ID, sku); err != nil {
				t.Fatal(err)
			}
			now, reference := repo.now(), uuid.NewString()
			row := ProviderBillEntry{EntryType: "PAYMENT", ProviderReferenceID: reference, ChannelTransactionID: reference, MerchantOrderNo: order.MerchantOrderNo, Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, Status: status, PaidAt: now}
			if status == "lowercase-channel" {
				row.Status = "SUCCESS"
				row.Channel = strings.ToLower(row.Channel)
			}
			supplier := &reconciliationFixtureProvider{retainedFinancialProvider: retainedFinancialProvider{result: ProviderOrder{MerchantOrderNo: order.MerchantOrderNo, Status: "SUCCESS", Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, ChannelTransactionID: &reference, PaidAt: &now, ExternalCustomerKey: order.RequesterID.String(), ExternalSKU: sku}}}
			coordinator, err := NewPaymentCoordinatorWithProvider(repo, supplier, "test")
			if err != nil {
				t.Fatal(err)
			}
			local := now.In(time.FixedZone("Asia/Shanghai", 8*60*60))
			date := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
			run, err := coordinator.ImportProviderBill(ctx, uuid.New(), order.Channel, date, []ProviderBillEntry{row})
			if err != nil {
				t.Fatal("accepted status spelling rejected", err)
			}
			wallet, err := repo.GetWallet(ctx, order.WalletID)
			if err != nil || supplier.queryCount != 1 || run.CaseCount != 0 || wallet.PostedMicrocredits != 1000000 {
				t.Fatalf("accepted successful spelling skipped actual authority: queries=%d cases=%d posted=%d", supplier.queryCount, run.CaseCount, wallet.PostedMicrocredits)
			}
		})
	}
}
