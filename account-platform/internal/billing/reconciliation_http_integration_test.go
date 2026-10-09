package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

// Real Billing handlers and PostgreSQL, with controlled human decoding only.
// Permission/OIDC middleware and live vendor acceptance are separate scopes.
func TestF03AdminReconciliationExactWire(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	c, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, true)
	base := c.provider.(*retainedFinancialProvider)
	provider := &reconciliationFixtureProvider{retainedFinancialProvider: *base}
	c, err := NewPaymentCoordinatorWithProvider(r, provider, "test")
	if err != nil {
		t.Fatal(err)
	}
	actor := uuid.New()
	s := NewService(r).WithPayments(c)
	call := func(handler http.HandlerFunc, path string, body string, key string, owner uuid.UUID) *httptest.ResponseRecorder {
		method := http.MethodPost
		if body == "" {
			method = http.MethodGet
		}
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.SetPathValue("case_id", key)
		request.Header.Set("Authorization", "Bearer controlled-human")
		if strings.HasSuffix(path, "/commands") {
			request.Header.Set("Idempotency-Key", "controlled-case-original-key")
		}
		out := httptest.NewRecorder()
		identity.Middleware(func(context.Context, string) (identity.Principal, error) {
			return identity.Principal{Issuer: "controlled", Subject: owner.String()}, nil
		}, handler).ServeHTTP(out, request)
		return out
	}
	now := r.now()
	local := now.In(time.FixedZone("Asia/Shanghai", 8*60*60))
	date := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC).Format("2006-01-02")
	row := ProviderBillEntry{EntryType: "PAYMENT", MerchantOrderNo: order.MerchantOrderNo, ChannelTransactionID: *base.result.ChannelTransactionID, ProviderReferenceID: *base.result.ChannelTransactionID, Channel: order.Channel, AmountFen: 9007199254740993, Currency: order.Currency, Status: "SUCCESS", PaidAt: now}
	body, err := json.Marshal(map[string]any{"channel": order.Channel, "business_date": date, "items": []ProviderBillEntry{row}})
	if err != nil {
		t.Fatal(err)
	}
	imported := call(s.adminImportReconciliation, "/admin/v1/reconciliation-imports", string(body), "", actor)
	if imported.Code != http.StatusCreated {
		t.Fatal("actual import handler rejected legal int64 row", imported.Code)
	}
	var run ReconciliationRun
	if err = json.Unmarshal(imported.Body.Bytes(), &run); err != nil || run.CaseCount != 1 {
		t.Fatal("actual import did not create amount case", err)
	}
	// The UI selects a Shanghai calendar day; pagination is half-open.
	dateValue, err := time.Parse("2006-01-02", date)
	if err != nil {
		t.Fatal(err)
	}
	runsPath := "/admin/v1/reconciliation-runs?channel=" + order.Channel + "&business_date_from=" + date + "&business_date_to="
	invalidRuns := call(s.adminReconciliationRuns, runsPath+date, "", "", actor)
	if invalidRuns.Code != http.StatusBadRequest || !strings.Contains(invalidRuns.Body.String(), `"code":"INVALID_FILTER"`) {
		t.Fatal("same-date range bypassed actual half-open parser", invalidRuns.Code)
	}
	validRuns := call(s.adminReconciliationRuns, runsPath+dateValue.AddDate(0, 0, 1).Format("2006-01-02"), "", "", actor)
	if validRuns.Code != http.StatusOK || !strings.Contains(validRuns.Body.String(), run.ID.String()) {
		t.Fatal("actual single-day range did not return persisted run", validRuns.Code)
	}
	items, err := r.AdminReconciliationCases(ctx, run.ID, 20)
	if err != nil || len(items) != 1 || items[0].CaseType != "AMOUNT_MISMATCH" {
		t.Fatal("actual mismatch case missing", err)
	}
	page := call(s.adminReconciliationCases, "/admin/v1/reconciliation-cases?run_id="+run.ID.String(), "", "", actor)
	if page.Code != 200 || !strings.Contains(page.Body.String(), `"amount_fen":"9007199254740993"`) {
		t.Fatal("GET did not project exact persisted money", page.Code)
	}
	var actualRowAmount int64
	if err = r.pool.QueryRow(ctx, `SELECT (provider_snapshot->>'amount_fen')::bigint FROM reconciliation_cases WHERE id=$1`, items[0].ID).Scan(&actualRowAmount); err != nil || actualRowAmount != 9007199254740993 {
		t.Fatal("read projection rewrote stored numerical facts", err)
	}
	commandBody := `{"action":"CLAIM","reason":"controlled reconciliation claim reason"}`
	path := "/admin/v1/reconciliation-cases/" + items[0].ID.String() + "/commands"
	first := call(s.adminUpdateReconciliationCase, path, commandBody, items[0].ID.String(), actor)
	if first.Code != 200 || !strings.Contains(first.Body.String(), `"amount_fen":"9007199254740993"`) {
		t.Fatal("command response lost exact money", first.Code)
	}
	if _, err = r.UpdateReconciliationCase(ctx, items[0].ID, ReconciliationCaseCommand{Action: "IGNORE", ActorID: actor, IdempotencyKey: uuid.NewString(), Reason: "controlled final review reason", ReviewResult: "confirmed actual difference"}); err != nil {
		t.Fatal(err)
	}
	replay := call(s.adminUpdateReconciliationCase, path, commandBody, items[0].ID.String(), actor)
	if replay.Code != 200 || replay.Body.String() != first.Body.String() {
		t.Fatal("HTTP replay changed frozen receipt", replay.Code)
	}
	other := call(s.adminUpdateReconciliationCase, path, commandBody, items[0].ID.String(), uuid.New())
	if other.Code != http.StatusConflict {
		t.Fatal("another actor adopted original receipt", other.Code)
	}
	if filename := os.Getenv("F03_WIRE_FIXTURE"); filename != "" {
		data, _ := json.Marshal(map[string]json.RawMessage{"run": imported.Body.Bytes(), "casePage": page.Body.Bytes(), "case": first.Body.Bytes()})
		f, err := os.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal("private fixture output already exists or unavailable")
		}
		_, writeErr := f.Write(data)
		closeErr := f.Close()
		if writeErr != nil || closeErr != nil {
			t.Fatal("private fixture output failed")
		}
	}
}

func TestF03ImportHTTPPendingReceiptResumesSameDurableRun(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	_, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
	now := r.now()
	ref := uuid.NewString()
	actor := uuid.New()
	row := ProviderBillEntry{EntryType: "PAYMENT", ProviderReferenceID: ref, ChannelTransactionID: ref, MerchantOrderNo: order.MerchantOrderNo, Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, Status: "SUCCESS", PaidAt: now}
	provider := &recoveryBillProvider{reconciliationFixtureProvider: reconciliationFixtureProvider{rows: []ProviderBillEntry{row}}, failing: map[string]bool{order.MerchantOrderNo: true}, orders: map[string]ProviderOrder{order.MerchantOrderNo: {MerchantOrderNo: order.MerchantOrderNo, Status: "SUCCESS", Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, ChannelTransactionID: &ref, PaidAt: &now, ExternalCustomerKey: order.RequesterID.String()}}}
	c, err := NewPaymentCoordinatorWithProvider(r, provider, "test")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(r).WithPayments(c)
	raw, _ := json.Marshal(map[string]any{"channel": order.Channel, "business_date": normalizedReconciliationBusinessDate(now).Format("2006-01-02"), "items": []ProviderBillEntry{row}})
	call := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/admin/v1/reconciliation-imports", strings.NewReader(string(raw)))
		request.Header.Set("Authorization", "Bearer controlled-human")
		out := httptest.NewRecorder()
		identity.Middleware(func(context.Context, string) (identity.Principal, error) {
			return identity.Principal{Issuer: "controlled", Subject: actor.String()}, nil
		}, http.HandlerFunc(service.adminImportReconciliation)).ServeHTTP(out, request)
		return out
	}
	pending := call()
	var errorReceipt struct {
		Code    string `json:"code"`
		Details struct {
			RunID uuid.UUID `json:"reconciliation_run_id"`
		} `json:"details"`
	}
	if err := json.Unmarshal(pending.Body.Bytes(), &errorReceipt); err != nil || pending.Code != 503 || errorReceipt.Code != "COMMAND_RESULT_UNKNOWN" || errorReceipt.Details.RunID == uuid.Nil {
		t.Fatal("pending response lost durable identity", pending.Code, pending.Body.String(), err)
	}
	var completedAt *time.Time
	if err := r.pool.QueryRow(ctx, `SELECT completed_at FROM reconciliation_runs WHERE id=$1`, errorReceipt.Details.RunID).Scan(&completedAt); err != nil || completedAt != nil {
		t.Fatal("pending completion timestamp is fabricated", completedAt, err)
	}
	provider.failing[order.MerchantOrderNo] = false
	success := call()
	var run ReconciliationRun
	if err := json.Unmarshal(success.Body.Bytes(), &run); err != nil || success.Code != 201 || run.ID != errorReceipt.Details.RunID || run.Status != "COMPLETED" || run.CompletedAt == nil {
		t.Fatal("same file did not recover stable success receipt", success.Code, success.Body.String(), err)
	}
	calls := provider.calls[order.MerchantOrderNo]
	replay := call()
	if replay.Code != 201 || replay.Body.String() != success.Body.String() || provider.calls[order.MerchantOrderNo] != calls {
		t.Fatal("completed import receipt not stable")
	}
	wallet, err := r.GetWallet(ctx, order.WalletID)
	if err != nil || wallet.PostedMicrocredits != 1000000 {
		t.Fatal("HTTP recovery monetary effect incorrect", err)
	}
}
