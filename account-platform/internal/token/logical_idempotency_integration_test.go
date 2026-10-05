package token

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	billingdomain "github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"
	"github.com/google/uuid"
)

func TestPostgresHTTPLogicalModelCallsHaveDistinctKeysAndRetryOneSettlement(t *testing.T) {
	ctx := context.Background()
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"usage":{"prompt_tokens":1,"completion_tokens":0}}`)
	}))
	defer upstream.Close()
	service, repo, _ := proxyFixture(t, upstream.URL)
	resolved, _ := service.contexts.Resolve(ctx, "")
	pool := testdb.Open(t, "billing")
	config := pool.Config()
	dsn, err := url.Parse(config.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	query := dsn.Query()
	query.Set("search_path", config.ConnConfig.RuntimeParams["search_path"])
	dsn.RawQuery = query.Encode()
	billingRepo, err := billingdomain.NewPostgresRepository(ctx, dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	defer billingRepo.Close()
	wallet, _, err := billingRepo.EnsureWallet(ctx, "PERSONAL", resolved.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := billingRepo.Credit(ctx, wallet.ID, "logical-call-fixture", 1000, true); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.pool.Exec(ctx, "UPDATE token_accounts SET wallet_id=$1", wallet.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO pricing_versions(id,model,input_per_million,output_per_million,cached_input_per_million,effective_from,created_at) VALUES($1,'model-test',1000000,0,0,$2,$2)`, uuid.New(), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	serviceToken := strings.Repeat("r", 32)
	billingServer := httptest.NewServer(billingdomain.NewService(billingRepo).Handler(serviceToken))
	defer billingServer.Close()
	service.billing, err = NewHTTPBillingClient(billingServer.URL, serviceToken)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(service.Handler(func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "https://fixture.example", Subject: "fixture", SessionID: resolved.SessionID}, nil
	}))
	defer server.Close()
	call := func(key, body string) int {
		t.Helper()
		request, _ := http.NewRequest("POST", server.URL+"/v1/chat/completions", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer fixture-auth")
		if key != "" {
			request.Header.Set("Idempotency-Key", key)
		}
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal("actual Gateway HTTP request failed")
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		return response.StatusCode
	}
	body := `{"model":"model-test","messages":[{"role":"user","content":"same prompt"}]}`
	for _, key := range []string{"", "too-short", strings.Repeat("a", 129)} {
		if call(key, body) != http.StatusBadRequest {
			t.Fatal("missing or invalid logical operation key was accepted")
		}
		if call(key, `{"model":"unconfigured-model"}`) != http.StatusBadRequest {
			t.Fatal("route or pricing failure bypassed required logical operation key validation")
		}
	}
	var requests, holds int
	if err := repo.pool.QueryRow(ctx, "SELECT count(*) FROM usage_requests").Scan(&requests); err != nil || requests != 0 {
		t.Fatal("invalid key persisted model usage")
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM usage_holds").Scan(&holds); err != nil || holds != 0 || upstreamCalls.Load() != 0 {
		t.Fatal("invalid key reserved funds or requested the model")
	}
	first, second := uuid.NewString(), uuid.NewString()
	if call(first, body) != http.StatusOK || call(second, body) != http.StatusOK {
		t.Fatal("two intentional identical prompts could not use distinct logical keys")
	}
	if call(first, body) != http.StatusConflict {
		t.Fatal("same-key transport retry did not block duplicate model consumption")
	}
	if call(first, `{"model":"model-test","messages":[{"role":"user","content":"changed prompt"}]}`) != http.StatusConflict {
		t.Fatal("same-key changed request facts were accepted")
	}
	if upstreamCalls.Load() != 2 {
		t.Fatal("retry or conflicting request sent duplicate upstream consumption")
	}
	balance, err := billingRepo.GetWallet(ctx, wallet.ID)
	if err != nil || balance.PostedMicrocredits != 998 || balance.ReservedMicrocredits != 0 {
		t.Fatal("logical calls did not settle each real Billing hold exactly once")
	}
	var terminal int
	if err := repo.pool.QueryRow(ctx, "SELECT count(*) FROM usage_requests WHERE status='COMMITTED' AND actual_microcredits=1").Scan(&terminal); err != nil || terminal != 2 {
		t.Fatal("logical call terminal records were missing or duplicated")
	}
	var events int
	if err := repo.pool.QueryRow(ctx, "SELECT count(*) FROM outbox_events WHERE event_type='token.usage.settled.v1'").Scan(&events); err != nil || events != 2 {
		t.Fatal("logical operation retries duplicated terminal events")
	}
	// The durable wire facts use exact request hashes, not a fabricated model result.
	var snapshot []byte
	if err := repo.pool.QueryRow(ctx, "SELECT request_snapshot FROM usage_requests WHERE idempotency_key=$1", first).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	var requestFacts map[string]any
	if json.Unmarshal(snapshot, &requestFacts) != nil || requestFacts["request_sha256"] == nil {
		t.Fatal("logical operation lost its original request facts")
	}
}
