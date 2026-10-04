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

func TestPostgresSettlementStatusDistinguishesFixedBoundFromUnknownCommit(t *testing.T) {
	for _, test := range []struct {
		name            string
		stream          bool
		input           int64
		loseCommitReply bool
	}{
		{name: "overbound-json", input: 1000},
		{name: "overbound-sse", stream: true, input: 1000},
		{name: "durable-commit-response-lost", input: 50, loseCommitReply: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				payload, _ := json.Marshal(map[string]any{"usage": map[string]int64{"prompt_tokens": test.input, "completion_tokens": 0}})
				if test.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: "+string(payload)+"\n\ndata: [DONE]\n\n")
				} else {
					w.Write(payload)
				}
			}))
			defer upstream.Close()
			s, tokenRepo, _ := proxyFixture(t, upstream.URL)
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
			resolved, err := s.contexts.Resolve(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			wallet, _, err := billingRepo.EnsureWallet(ctx, "PERSONAL", resolved.UserID)
			if err != nil {
				t.Fatal(err)
			}
			const initial = int64(10000)
			if _, err := billingRepo.Credit(ctx, wallet.ID, "settlement-status-fixture", initial, true); err != nil {
				t.Fatal(err)
			}
			price := PriceVersion{ID: uuid.New(), Model: "model-test", InputPerMillion: 1_000_000}
			if _, err := pool.Exec(ctx, `INSERT INTO pricing_versions(id,model,input_per_million,output_per_million,cached_input_per_million,effective_from,created_at) VALUES($1,$2,$3,0,0,$4,$4)`, price.ID, price.Model, price.InputPerMillion, time.Now().Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
			if _, err := tokenRepo.pool.Exec(ctx, `UPDATE token_accounts SET wallet_id=$1`, wallet.ID); err != nil {
				t.Fatal(err)
			}
			billingHandler := billingdomain.NewService(billingRepo).Handler(strings.Repeat("s", 32))
			var commitCalls atomic.Int32
			billingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/internal/v1/holds/commit" {
					call := commitCalls.Add(1)
					if test.loseCommitReply && call == 1 {
						// Persist the real Billing terminal, then hide its response to
						// exercise an uncertain HTTP outcome at the Token boundary.
						response := httptest.NewRecorder()
						billingHandler.ServeHTTP(response, r)
						if response.Code != http.StatusOK {
							t.Error("fixture did not durably commit Billing before hiding its response")
						}
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
				}
				billingHandler.ServeHTTP(w, r)
			}))
			defer billingServer.Close()
			s.billing, err = NewHTTPBillingClient(billingServer.URL, strings.Repeat("s", 32))
			if err != nil {
				t.Fatal(err)
			}
			gateway := httptest.NewServer(s.Handler(func(context.Context, string) (identity.Principal, error) { return identity.Principal{}, nil }))
			defer gateway.Close()
			requestBody, _ := json.Marshal(map[string]any{"model": "model-test", "stream": test.stream})
			request, _ := http.NewRequest(http.MethodPost, gateway.URL+"/v1/chat/completions", strings.NewReader(string(requestBody)))
			request.Header.Set("Authorization", "Bearer test-only-access-token")
			request.Header.Set("Idempotency-Key", uuid.NewString())
			response, err := gateway.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			_, readErr := io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if readErr != nil || response.StatusCode != http.StatusOK {
				t.Fatalf("model response status=%d read=%v", response.StatusCode, readErr)
			}
			var usageID uuid.UUID
			var holdID uuid.UUID
			var status, reason string
			var maximum int64
			if err := tokenRepo.pool.QueryRow(ctx, `SELECT id,hold_id,status,error_class,maximum_microcredits FROM usage_requests`).Scan(&usageID, &holdID, &status, &reason, &maximum); err != nil {
				t.Fatal(err)
			}
			var holdStatus string
			if err := pool.QueryRow(ctx, `SELECT status FROM usage_holds WHERE id=$1`, holdID).Scan(&holdStatus); err != nil {
				t.Fatal(err)
			}
			wantDisplay := "MANUAL_REVIEW"
			wantPosted, wantReserved := initial, int64(100)
			if test.loseCommitReply {
				wantDisplay, wantPosted, wantReserved = "SETTLEMENT_PENDING", initial-test.input, 0
				if status != "SETTLEMENT_PENDING" || reason != "settlement_pending" || holdStatus != "COMMITTED" {
					t.Fatal("unknown commit result did not remain settlement pending")
				}
			} else if status != "REVIEW" || reason != "usage_exceeds_reservation" || maximum != 100 || holdStatus != "ACTIVE" || commitCalls.Load() != 0 {
				t.Fatal("fixed-bound violation was retried as an ordinary settlement or changed Billing funds")
			}
			statusRequest, _ := http.NewRequest(http.MethodGet, gateway.URL+"/v1/usage-requests/"+usageID.String(), nil)
			statusRequest.Header.Set("Authorization", "Bearer test-only-access-token")
			statusResponse, err := gateway.Client().Do(statusRequest)
			if err != nil {
				t.Fatal(err)
			}
			var displayed struct {
				DisplayStatus string `json:"display_status"`
			}
			decodeErr := json.NewDecoder(statusResponse.Body).Decode(&displayed)
			statusResponse.Body.Close()
			if decodeErr != nil || statusResponse.StatusCode != http.StatusOK || displayed.DisplayStatus != wantDisplay {
				t.Fatal("production usage-status endpoint did not preserve the settlement classification")
			}
			if test.loseCommitReply {
				// Exercise existing convergence after Billing is available, without
				// introducing a recovery worker or sending the model request again.
				s.settle(ctx, usageID, holdID, maximum, price, Usage{InputTokens: test.input})
				settled, err := tokenRepo.GetUsage(ctx, usageID)
				if err != nil || settled.Status != "COMMITTED" || settled.ActualMicrocredits == nil || *settled.ActualMicrocredits != test.input || commitCalls.Load() != 2 {
					t.Fatal("pending settlement did not converge by idempotent Billing confirmation")
				}
			}
			current, err := billingRepo.GetWallet(ctx, wallet.ID)
			if err != nil || current.PostedMicrocredits != wantPosted || current.ReservedMicrocredits != wantReserved {
				t.Fatal("settlement classification changed the durable wallet invariants")
			}
		})
	}
}
