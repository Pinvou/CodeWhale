package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/paymentsimulator"
	"github.com/google/uuid"
)

// Public Billing HTTP -> actual simulator adapter -> controlled upstream HTTP
// -> complete public PG schema under billing_service. This is not a live vendor
// or the simulator's own persistence acceptance. Unknown source kinds cannot
// become a payment/refund/chargeback financial fact.
func TestReconciliationUnknownProviderEntryPublicPostgres(t *testing.T) {
	f06WalletActors(t)
	r, ctx := retainedFinanceRepository(t)
	_, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
	const sku = "controlled-unknown-entry-sku"
	if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET provider_id='development_simulator',provider_version='development-simulator-v1',provider_sku=$2 WHERE id=$1`, order.ID, sku); err != nil {
		t.Fatal(err)
	}
	now, transaction := r.now(), uuid.NewString()
	good := ProviderBillEntry{EntryType: "PAYMENT", ProviderReferenceID: transaction, ChannelTransactionID: transaction, MerchantOrderNo: order.MerchantOrderNo, Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, Status: "SUCCESS", PaidAt: now}
	unknown := good
	unknown.EntryType = "FUTURE_FEE"
	unknown.ProviderReferenceID = "unknown-" + uuid.NewString()
	unknown.ChannelTransactionID = unknown.ProviderReferenceID
	unknown.MerchantOrderNo = "unbound-" + uuid.NewString()
	unknown.AmountFen = 777
	rows := []ProviderBillEntry{unknown, good}
	var bills, queries, writes atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.Header.Get("Authorization") != "Bearer controlled-provider" {
			t.Error("upstream credential missing")
			w.WriteHeader(403)
			return
		}
		if q.Method != "GET" {
			writes.Add(1)
			w.WriteHeader(405)
			return
		}
		switch {
		case strings.HasPrefix(q.URL.Path, "/internal/v1/bills/"):
			bills.Add(1)
			if q.URL.Query().Get("channel") != order.Channel {
				t.Error("wrong bill channel")
			}
			json.NewEncoder(w).Encode(map[string]any{"items": rows})
		case q.URL.Path == "/internal/v1/orders/"+order.MerchantOrderNo:
			queries.Add(1)
			json.NewEncoder(w).Encode(ProviderOrder{MerchantOrderNo: order.MerchantOrderNo, Status: "SUCCESS", Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, ChannelTransactionID: &transaction, PaidAt: &now, ExternalCustomerKey: order.RequesterID.String(), ExternalSKU: sku})
		default:
			t.Error("unknown entry caused an upstream query", q.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	provider, err := paymentsimulator.New(upstream.URL, "controlled-provider")
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := NewPaymentCoordinatorWithProvider(r, provider, "test")
	if err != nil {
		t.Fatal(err)
	}
	actor := uuid.New()
	authority := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		var in struct{ Permission, Subject string }
		if json.NewDecoder(q.Body).Decode(&in) != nil || in.Permission != "platform.billing.reconciliation.run" || in.Subject != actor.String() {
			t.Error("wrong reconciliation authority request")
		}
		json.NewEncoder(w).Encode(map[string]any{"allowed": true, "user_id": actor})
	}))
	defer authority.Close()
	authorizer, err := authorization.NewClient(authority.URL, "controlled-authority")
	if err != nil {
		t.Fatal(err)
	}
	handler := NewService(r).WithPayments(coordinator).WithAuthorizer(authorizer).Handler("controlled-authority", func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "controlled-reconciliation", Subject: actor.String()}, nil
	})
	call := func() *httptest.ResponseRecorder {
		raw, _ := json.Marshal(map[string]string{"channel": order.Channel, "business_date": reconciliationFixtureDateForPaidAt(now).Format("2006-01-02")})
		q := httptest.NewRequest("POST", "/admin/v1/reconciliation-runs", strings.NewReader(string(raw))).WithContext(ctx)
		q.Header.Set("Authorization", "Bearer controlled-human")
		q.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, q)
		return out
	}
	var run ReconciliationRun
	t.Run("api-unknown-first-does-not-poison-known-payment", func(t *testing.T) {
		out := call()
		if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &run) != nil || run.Status != "COMPLETED" || run.CaseCount != 1 {
			t.Fatal("batch did not complete", out.Code, out.Body.String())
		}
		cases, err := r.AdminReconciliationCases(ctx, run.ID, 10)
		if err != nil || len(cases) != 1 || cases[0].CaseType != "UNSUPPORTED_PROVIDER_ENTRY" || cases[0].Status != "OPEN" {
			t.Fatal("manual case missing", cases, err)
		}
		var stored string
		var rowCount, evidence, journals int
		if err := r.pool.QueryRow(ctx, `SELECT (SELECT entry_type FROM reconciliation_provider_rows WHERE reconciliation_run_id=$1 AND row_number=1),(SELECT count(*) FROM reconciliation_provider_rows WHERE reconciliation_run_id=$1),(SELECT count(*) FROM payment_evidence WHERE evidence_type='BILL' AND payload_snapshot->>'entry_type'='FUTURE_FEE'),(SELECT count(*) FROM journal_entries WHERE entry_type='RECHARGE')`, run.ID).Scan(&stored, &rowCount, &evidence, &journals); err != nil || stored != "FUTURE_FEE" || rowCount != 2 || evidence != 1 || journals != 1 {
			t.Fatal("source evidence or known posting missing", stored, rowCount, evidence, journals, err)
		}
		var postings int
		var liability, clearing int64
		err = r.pool.QueryRow(ctx, `SELECT count(*),COALESCE(sum(p.delta_microcredits) FILTER(WHERE a.wallet_id=$2),0),COALESCE(sum(p.delta_microcredits) FILTER(WHERE a.id=$3),0) FROM ledger_postings p JOIN ledger_accounts a ON a.id=p.ledger_account_id JOIN journal_entries j ON j.id=p.journal_entry_id WHERE j.business_reference='payment-transaction:'||(SELECT id::text FROM payment_transactions WHERE payment_order_id=$1)`, order.ID, order.WalletID, paymentClearingAccount).Scan(&postings, &liability, &clearing)
		if err != nil || postings != 2 || liability != 1000000 || clearing != -1000000 {
			t.Fatal("known row posting owner/direction mismatch", postings, liability, clearing, err)
		}
		wallet, err := r.GetWallet(ctx, order.WalletID)
		if err != nil || wallet.PostedMicrocredits != 1000000 || wallet.ReservedMicrocredits != 0 || queries.Load() != 1 || bills.Load() != 1 || writes.Load() != 0 {
			t.Fatal("unknown kind changed money or probed provider", wallet, queries.Load(), bills.Load(), writes.Load(), err)
		}
	})
	t.Run("retry-retains-one-case-and-one-financial-effect", func(t *testing.T) {
		before := waiverFixtureSnapshot(t, r, ctx)
		out := call()
		var again ReconciliationRun
		if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &again) != nil || again.Status != "COMPLETED" || again.CaseCount != 1 {
			t.Fatal("retry still poisoned", out.Code, out.Body.String())
		}
		var count int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM reconciliation_cases WHERE case_type='UNSUPPORTED_PROVIDER_ENTRY'`).Scan(&count); err != nil || count != 1 {
			t.Fatal("duplicate unsupported case", count, err)
		}
		if before != waiverFixtureSnapshot(t, r, ctx) || writes.Load() != 0 {
			t.Fatal("repeat observation changed financial facts")
		}
	})
	t.Run("oversized-kind-rejected-before-batch-write", func(t *testing.T) {
		rows[0].EntryType = strings.Repeat("X", 65)
		var before, after int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM reconciliation_runs`).Scan(&before); err != nil {
			t.Fatal(err)
		}
		out := call()
		if out.Code == 200 {
			t.Fatal("oversized source accepted")
		}
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM reconciliation_runs`).Scan(&after); err != nil || after != before {
			t.Fatal("invalid row left batch", before, after, err)
		}
	})
	t.Run("file-import-unknown-kind-keeps-existing-contract", func(t *testing.T) {
		fileProvider := &reconciliationFixtureProvider{}
		file, err := NewPaymentCoordinatorWithProvider(r, fileProvider, "test")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.ImportProviderBill(ctx, actor, order.Channel, reconciliationFixtureDateForPaidAt(now), []ProviderBillEntry{unknown}); !errors.Is(err, ErrConflict) {
			t.Fatal("file import contract widened", err)
		}
	})
}
