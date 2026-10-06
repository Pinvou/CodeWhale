package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/paymentsimulator"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

type adminRefundProvider struct {
	*recoverableProvider
	mu           sync.Mutex
	refunds      map[string]ProviderRefund
	loseResponse bool
}

type financialSnapshotProvider struct {
	*recoverableProvider
	entries []ProviderBillEntry
}

func (p financialSnapshotProvider) Bill(context.Context, string, time.Time) ([]ProviderBillEntry, error) {
	return p.entries, nil
}

func TestPostgresAdminReconciliationSnapshotReadsPreserveExactFinancialStrings(t *testing.T) {
	ctx := context.Background()
	p, wallet, user := paymentFixture(t)
	order := creditedOrder(t, p, wallet, user, uuid.NewString())
	p.provider = financialSnapshotProvider{recoverableProvider: &recoverableProvider{}, entries: []ProviderBillEntry{{MerchantOrderNo: order.MerchantOrderNo, ChannelTransactionID: *order.PrimaryProviderTransactionID, Channel: order.Channel, AmountFen: 9007199254740993, Currency: order.Currency, Status: "SUCCESS", PaidAt: *order.PaidAt}}}
	date := time.Date(order.PaidAt.Year(), order.PaidAt.Month(), order.PaidAt.Day(), 0, 0, 0, 0, time.UTC)
	run, err := p.RunReconciliation(ctx, order.Channel, date)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewService(p.repository).WithAuthorizer(billingTestAuthorizer(t)).WithPayments(p).Handler(strings.Repeat("f", 32), func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "https://fixture-authority.invalid", Subject: "fixture-platform-operator", RealmRoles: []string{"platform-admin"}}, nil
	})
	request := httptest.NewRequest("GET", "/admin/v1/reconciliation-runs/"+run.ID.String()+"/cases", nil)
	request.Header.Set("Authorization", "Bearer fixture-admin")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var body struct {
		Items []struct {
			Provider map[string]any `json:"provider_snapshot"`
			Internal map[string]any `json:"internal_snapshot"`
		}
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &body) != nil || len(body.Items) != 1 {
		t.Fatal("snapshot fixture did not reach the admin handler")
	}
	item := body.Items[0]
	if item.Provider["amount_fen"] != "9007199254740993" || item.Internal["payment_order"].(map[string]any)["amount_fen"] != strconv.FormatInt(order.AmountFen, 10) || item.Internal["payment_order"].(map[string]any)["credit_microcredits"] != strconv.FormatInt(order.CreditMicrocredits, 10) || item.Internal["payment_transaction"].(map[string]any)["amount_fen"] != strconv.FormatInt(order.AmountFen, 10) {
		t.Fatal("admin reconciliation evidence lost exact money")
	}
	var providerType, internalType string
	if err := p.repository.pool.QueryRow(ctx, `SELECT jsonb_typeof(provider_snapshot->'amount_fen'),jsonb_typeof(internal_snapshot->'payment_order'->'amount_fen') FROM reconciliation_cases WHERE reconciliation_run_id=$1`, run.ID).Scan(&providerType, &internalType); err != nil || providerType != "number" || internalType != "number" {
		t.Fatal("new read DTO rewrote the existing financial snapshot storage")
	}
	var identity struct {
		Items []struct {
			ID uuid.UUID `json:"case_id"`
		}
	}
	if json.Unmarshal(response.Body.Bytes(), &identity) != nil || len(identity.Items) != 1 {
		t.Fatal("case identity absent")
	}
	key := uuid.NewString()
	for _, stage := range []string{"first", "replay"} {
		t.Run(stage, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/admin/v1/reconciliation-cases/"+identity.Items[0].ID.String()+"/commands", strings.NewReader(`{"action":"CLAIM","reason":"controlled exact financial review"}`))
			request.Header.Set("Authorization", "Bearer fixture-admin")
			request.Header.Set("Idempotency-Key", key)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			var result struct {
				Provider map[string]any `json:"provider_snapshot"`
				Internal map[string]any `json:"internal_snapshot"`
			}
			if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil {
				t.Fatal("command receipt unavailable")
			}
			if result.Provider["amount_fen"] != "9007199254740993" || result.Internal["payment_order"].(map[string]any)["amount_fen"] != strconv.FormatInt(order.AmountFen, 10) {
				t.Fatal("command receipt lost exact read money")
			}
		})
	}

}

func TestPostgresAdminLedgerRetainsDistinctPostingsFromOneJournal(t *testing.T) {
	ctx := context.Background()
	repo, wallet := billingFixture(t)
	for _, ref := range []string{"first-ledger-lot", "second-ledger-lot"} {
		if _, err := repo.Credit(ctx, wallet.ID, ref, 100, true); err != nil {
			t.Fatal(err)
		}
	}
	hold, err := repo.Reserve(ctx, uuid.New(), wallet.ID, 150, nil, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit(ctx, hold.UsageRequestID, hold.ID, 150, map[string]any{"fixture": true}); err != nil {
		t.Fatal(err)
	}
	handler := NewService(repo).WithAuthorizer(billingTestAuthorizer(t)).Handler(strings.Repeat("f", 32), func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "https://fixture-authority.invalid", Subject: "fixture-platform-operator", RealmRoles: []string{"platform-admin"}}, nil
	})
	request := httptest.NewRequest("GET", "/admin/v1/wallets/"+wallet.ID.String()+"/ledger", nil)
	request.Header.Set("Authorization", "Bearer fixture-admin")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var body struct {
		Items []struct {
			PostingID uuid.UUID `json:"posting_id"`
			JournalID uuid.UUID `json:"journal_entry_id"`
			Amount    string    `json:"delta_microcredits"`
		}
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &body) != nil || len(body.Items) != 4 {
		t.Fatal("ledger omitted a wallet posting")
	}
	ids := map[uuid.UUID]bool{}
	debitJournals := map[uuid.UUID]int{}
	amounts := map[string]int{}
	for _, item := range body.Items {
		if item.PostingID == uuid.Nil || ids[item.PostingID] {
			t.Fatal("wallet postings share a UI identity")
		}
		ids[item.PostingID] = true
		amounts[item.Amount]++
		if strings.HasPrefix(item.Amount, "-") {
			debitJournals[item.JournalID]++
		}
	}
	if len(debitJournals) != 1 || amounts["100"] != 2 || amounts["-100"] != 1 || amounts["-50"] != 1 {
		t.Fatal("ledger changed immutable lot facts")
	}
	for _, count := range debitJournals {
		if count != 2 {
			t.Fatal("multi-lot debit fixture did not reach the production split")
		}
	}
	assertBalanced(t, repo)
}

func (p *adminRefundProvider) Refund(_ context.Context, _ string, merchant string, amount int64) (ProviderRefund, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if refund, ok := p.refunds[merchant]; ok {
		return refund, nil
	}
	refund := ProviderRefund{MerchantRefundNo: merchant, ChannelRefundID: uuid.NewString(), AmountFen: amount, Status: "SUCCESS"}
	p.refunds[merchant] = refund
	if p.loseResponse {
		p.loseResponse = false
		return ProviderRefund{}, errors.New("controlled lost response")
	}
	return refund, nil
}

func TestPostgresAdminRefundReusesLogicalKeyAfterProviderResponseLoss(t *testing.T) {
	p, w, user := paymentFixture(t)
	provider := &adminRefundProvider{recoverableProvider: &recoverableProvider{}, refunds: map[string]ProviderRefund{}, loseResponse: true}
	p.provider = provider
	order := creditedOrder(t, p, w, user, uuid.NewString())
	handler := NewService(p.repository).WithAuthorizer(billingTestAuthorizer(t)).WithPayments(p).Handler(strings.Repeat("f", 32), func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "https://fixture-authority.invalid", Subject: "fixture-platform-operator", RealmRoles: []string{"platform-admin"}}, nil
	})
	source, packageID := refundTargetFixture(t, p, order.ID)
	encodeBody := func(reason string) string {
		b, err := json.Marshal(map[string]any{"payment_transaction_id": source, "entitlement_package_id": packageID, "reason": reason})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	body := encodeBody("controlled refund")
	invoke := func(key, body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest("POST", "/admin/v1/payment-orders/"+order.ID.String()+"/refunds", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer fixture-auth")
		request.Header.Set("Idempotency-Key", key)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	for _, key := range []string{"", "too-short"} {
		if response := invoke(key, body); response.Code != 400 {
			t.Fatal("missing refund operation identity did not fail before a reservation")
		}
	}
	var count int
	if err := p.repository.pool.QueryRow(context.Background(), `SELECT count(*) FROM refunds`).Scan(&count); err != nil || count != 0 {
		t.Fatal("invalid keys created refunds")
	}
	key := uuid.NewString()
	response := invoke(key, body)
	var first map[string]any
	if response.Code != 201 || json.Unmarshal(response.Body.Bytes(), &first) != nil || first["amount_fen"] != strconv.FormatInt(order.AmountFen, 10) {
		t.Fatal("same-key recovery lost exact admin refund facts")
	}
	refundID, err := uuid.Parse(first["refund_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.ProcessRefund(context.Background(), refundID); !errors.Is(err, ErrPaymentProvider) {
		t.Fatal("controlled lost provider response was not retained", err)
	}
	if err := p.ProcessRefund(context.Background(), refundID); err != nil {
		t.Fatal("same merchant refund retry failed", err)
	}
	if err := p.ProcessRefund(context.Background(), refundID); err != nil {
		t.Fatal("confirmed refund replay failed", err)
	}
	response = invoke(key, body)
	var repeated map[string]any
	if response.Code != 201 || json.Unmarshal(response.Body.Bytes(), &repeated) != nil || repeated["refund_id"] != first["refund_id"] {
		t.Fatal("same logical retry created a second refund")
	}
	if response := invoke(key, encodeBody("different refund facts")); response.Code != 409 {
		t.Fatal("different refund facts reused the same operation identity")
	}
	if err := p.repository.pool.QueryRow(context.Background(), `SELECT count(*) FROM refunds`).Scan(&count); err != nil || count != 1 || len(provider.refunds) != 1 {
		t.Fatal("retry duplicated a durable or provider refund")
	}
	assertBalanced(t, p.repository)
}

func TestPostgresBillingOperationsKeepExactMoneyAndRejectInvalidJSON(t *testing.T) {
	repo, wallet := billingFixture(t)
	const amount int64 = 9007199254740993
	if _, err := repo.Credit(context.Background(), wallet.ID, "operations-funding", amount, true); err != nil {
		t.Fatal(err)
	}
	handler := NewService(repo).WithAuthorizer(billingTestAuthorizer(t)).Handler(strings.Repeat("f", 32), func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "https://fixture-authority.invalid", Subject: "fixture-platform-operator", RealmRoles: []string{"platform-admin"}}, nil
	})
	request := httptest.NewRequest("POST", "/admin/v1/pricing-versions", strings.NewReader(`{"model":"operations-model","input_per_million":9007199254740993,"output_per_million":1,"cached_input_per_million":2}`))
	request.Header.Set("Authorization", "Bearer fixture-auth")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var price map[string]any
	if response.Code != 201 || json.Unmarshal(response.Body.Bytes(), &price) != nil || price["input_per_million"] != "9007199254740993" {
		t.Fatal("new price version wire rounded a financial integer")
	}
	request = httptest.NewRequest("GET", "/admin/v1/wallets/"+wallet.ID.String()+"/ledger", nil)
	request.Header.Set("Authorization", "Bearer fixture-auth")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var ledger struct {
		Items []struct {
			Delta string `json:"delta_microcredits"`
		}
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &ledger) != nil || len(ledger.Items) != 1 || ledger.Items[0].Delta != "9007199254740993" {
		t.Fatal("new ledger wire rounded a financial integer")
	}
	for _, path := range []string{"/admin/v1/pricing-versions", "/admin/v1/reconciliation-runs"} {
		for _, body := range []string{"{", `{"unknown":true}`, `{"model":[]}`, `{} {}`, `{"model":"` + strings.Repeat("x", 1<<20) + `"}`} {
			request := httptest.NewRequest("POST", path, strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer fixture-auth")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_REQUEST"`) {
				t.Fatal("new billing admin invalid JSON did not return 400")
			}
		}
	}
	var count int
	if err := repo.pool.QueryRow(context.Background(), `SELECT count(*) FROM pricing_versions WHERE model='operations-model'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("invalid JSON changed price versions")
	}
	assertBalanced(t, repo)
}

func TestAdminFinancialSnapshotRetainsExactNestedMoney(t *testing.T) {
	body, err := adminFinancialSnapshot(json.RawMessage(`{"payment_order":{"amount_fen":9007199254740993,"credit_microcredits":9007199254740993,"summary":{"paid_amount_fen":"18446744073709551614"}},"postings":[{"delta_microcredits":-9007199254740993,"version":2}],"count":3}`))
	if err != nil {
		t.Fatal(err)
	}
	var actual map[string]any
	if err := json.Unmarshal(body, &actual); err != nil {
		t.Fatal(err)
	}
	order := actual["payment_order"].(map[string]any)
	posting := actual["postings"].([]any)[0].(map[string]any)
	if order["amount_fen"] != "9007199254740993" || order["credit_microcredits"] != "9007199254740993" || order["summary"].(map[string]any)["paid_amount_fen"] != "18446744073709551614" || posting["delta_microcredits"] != "-9007199254740993" || posting["version"] != float64(2) || actual["count"] != float64(3) {
		t.Fatal("nested financial facts were rounded or nonmoney fields changed")
	}
	if _, err := adminFinancialSnapshot(json.RawMessage(`{"nested":{"amount_fen":1.5}}`)); err == nil {
		t.Fatal("invalid stored monetary integer was accepted")
	}
}

func TestDailyPaymentWorkerDoesNotLogSupplierTransportAddress(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	address := server.URL
	server.Close()
	provider, err := paymentsimulator.New(address, "controlled-private-internal-test-token")
	if err != nil {
		t.Fatal("controlled constructor failed")
	}
	worker := NewDailyPaymentReconciliationWorker(&PaymentCoordinator{provider: provider})
	var captured bytes.Buffer
	worker.logger = slog.New(slog.NewTextHandler(&captured, nil))
	worker.runOnce(context.Background())
	if captured.Len() == 0 {
		t.Fatal("real failed supplier Bill did not reach worker logging")
	}
	if strings.Contains(captured.String(), address) || strings.Contains(captured.String(), "controlled-private-internal-test-token") {
		t.Fatal("worker exposed supplier transport details")
	}
	if !strings.Contains(captured.String(), "business_date") || !strings.Contains(captured.String(), "channel") {
		t.Fatal("safe scheduling facts disappeared")
	}
}
