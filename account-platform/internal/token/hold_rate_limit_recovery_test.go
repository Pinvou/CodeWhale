package token

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

type rateLimitedHoldRepository struct {
	streamRepositoryFake
	envelope DispatchEnvelope
}

func (r *rateLimitedHoldRepository) LoadDispatchPreparation(context.Context, uuid.UUID) (DispatchEnvelope, error) {
	return r.envelope, nil
}

func TestBillingRateLimitIsNotTerminalReservationEvidence(t *testing.T) {
	for _, failure := range []error{
		&BillingHTTPError{Status: http.StatusTooManyRequests, Code: "RATE_LIMITED"},
		fmt.Errorf("transport wrapper: %w", &BillingHTTPError{Status: http.StatusTooManyRequests, Code: "WALLET_INSUFFICIENT"}),
	} {
		if definitiveBillingFailure(failure) || definitiveInitialBillingFailure(failure) {
			t.Fatal("a transient rate limit cannot prove a definitive reservation rejection")
		}
	}
	if !definitiveInitialBillingFailure(&BillingHTTPError{Status: http.StatusPaymentRequired, Code: "WALLET_INSUFFICIENT"}) {
		t.Fatal("authoritative insufficient-funds rejection became uncertain")
	}
}

// Exercise the real HTTP client and replay handler. The repository seam only
// supplies the previously frozen preparation and records forbidden side effects;
// it does not classify Billing failures or decide the HTTP response.
func TestRateLimitedReservationReplayRetainsOriginalUsage(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	usageID, walletID := uuid.New(), uuid.New()
	var attempts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			UsageID  uuid.UUID `json:"usage_request_id"`
			WalletID uuid.UUID `json:"wallet_id"`
		}
		if r.Method != http.MethodPost || r.URL.Path != "/internal/v1/holds/reserve" ||
			json.NewDecoder(r.Body).Decode(&request) != nil || request.UsageID != usageID || request.WalletID != walletID {
			t.Error("reservation retry changed its frozen request identity")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		attempt := attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if attempt < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"code":"RATE_LIMITED","message":"retry later"}`))
			return
		}
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"code":"WALLET_INSUFFICIENT","message":"insufficient funds"}`))
	}))
	defer server.Close()
	client, err := NewHTTPBillingClient(server.URL, "controlled-rate-limit-test")
	if err != nil {
		t.Fatal(err)
	}
	upstream := NewHTTPUpstreamClient()
	snapshot, err := json.Marshal(frozenUsageSnapshot{ContextType: "PERSONAL", ProviderID: "controlled", AdapterVersion: upstream.AdapterVersion()})
	if err != nil {
		t.Fatal(err)
	}
	usage := UsageRequest{ID: usageID, WalletID: walletID, ExecutionStatus: "HOLD_RESERVING", RequestSnapshot: snapshot, MaximumMicrocredits: 100}
	repository := &rateLimitedHoldRepository{envelope: DispatchEnvelope{Provider: "controlled", AdapterVersion: upstream.AdapterVersion(), ContextType: "PERSONAL", Price: PriceVersion{ID: uuid.New(), FormulaVersionID: uuid.New()}}}
	repository.replaySnapshot = ReplaySnapshot{Usage: usage}
	service := &Service{repository: repository, billing: client, cancellationBilling: client, executionUpstream: upstream}
	for attempt := 1; attempt <= 2; attempt++ {
		response := httptest.NewRecorder()
		service.replayUsage(response, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), usage, nil)
		var body struct {
			Code           string    `json:"code"`
			UsageRequestID uuid.UUID `json:"usage_request_id"`
			Execution      string    `json:"execution_status"`
		}
		if response.Code != http.StatusAccepted || json.Unmarshal(response.Body.Bytes(), &body) != nil ||
			body.Code != "HOLD_RESULT_UNKNOWN" || body.UsageRequestID != usageID || body.Execution != "HOLD_RESERVING" ||
			response.Header().Get("Location") != "/v1/usage-requests/"+usageID.String() {
			t.Fatalf("rate limit became a terminal result: status=%d body=%s", response.Code, response.Body)
		}
		if attempts.Load() != int64(attempt) || repository.resultSaved || repository.releaseCalls != 0 || repository.reviewReason != "" {
			t.Fatal("rate limit persisted a false rejection or prevented the original retry")
		}
	}
	response := httptest.NewRecorder()
	service.replayUsage(response, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), usage, nil)
	if response.Code != http.StatusPaymentRequired || attempts.Load() != 3 || !repository.resultSaved || !repository.released || repository.savedResult.HTTPStatus != http.StatusPaymentRequired {
		t.Fatalf("later authoritative rejection did not close the same request: status=%d attempts=%d", response.Code, attempts.Load())
	}
}
