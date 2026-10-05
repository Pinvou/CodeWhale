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
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"
	"github.com/google/uuid"
)

// Evidence is a controlled New API HTTP fixture. Both Token and Billing use
// real PostgreSQL and production HTTP clients/routes; this is not a live model
// provider or production-pricing acceptance test.
func TestPostgresHTTPReconciliationPreservesFundsAndUsesHistoricalPrice(t *testing.T) {
	for _, scenario := range []string{"consumed", "channel-error", "delayed-consumption", "ambiguous", "overbound"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			tokenRepo, usage := settlementFixture(t)
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
			wallet, _, err := billingRepo.EnsureWallet(ctx, "PERSONAL", usage.UserID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := billingRepo.Credit(ctx, wallet.ID, "reconciliation-fixture", 100, true); err != nil {
				t.Fatal(err)
			}
			price := PriceVersion{ID: uuid.New(), Model: usage.Model, InputPerMillion: 1_000_000}
			if _, err := pool.Exec(ctx, `INSERT INTO pricing_versions(id,model,input_per_million,output_per_million,cached_input_per_million,effective_from,created_at) VALUES($1,$2,$3,0,0,now()-interval '1 hour',now())`, price.ID, price.Model, price.InputPerMillion); err != nil {
				t.Fatal(err)
			}
			hold, err := billingRepo.Reserve(ctx, usage.ID, wallet.ID, 100, &price.ID, time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tokenRepo.pool.Exec(ctx, `UPDATE usage_requests SET hold_id=$2,price_version_id=$3,wallet_id=$4 WHERE id=$1`, usage.ID, hold.ID, price.ID, wallet.ID); err != nil {
				t.Fatal(err)
			}
			requestID := "fixture-reconcile-request"
			if err := tokenRepo.RecordUpstreamRequest(ctx, usage.ID, requestID); err != nil {
				t.Fatal(err)
			}
			if err := tokenRepo.MarkReview(ctx, usage.ID, "fixture_uncertain", map[string]any{}); err != nil {
				t.Fatal(err)
			}
			serviceToken := strings.Repeat("r", 32)
			billingServer := httptest.NewServer(billingdomain.NewService(billingRepo).Handler(serviceToken))
			defer billingServer.Close()
			billingClient, err := NewHTTPBillingClient(billingServer.URL, serviceToken)
			if err != nil {
				t.Fatal(err)
			}
			var consumptionAvailable atomic.Bool
			evidenceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/log/" || r.URL.Query().Get("request_id") != requestID || r.Header.Get("Authorization") != "Bearer fixture-evidence-key" || r.Header.Get("New-Api-User") != "1" {
					t.Error("evidence client did not use the configured lookup contract")
					w.WriteHeader(400)
					return
				}
				items := []map[string]any{}
				if r.URL.Query().Get("type") == "2" && (scenario == "consumed" || scenario == "overbound" || (scenario == "delayed-consumption" && consumptionAvailable.Load())) {
					input := 5
					if scenario == "overbound" {
						input = 101
					}
					items = append(items, map[string]any{"request_id": requestID, "prompt_tokens": input, "completion_tokens": 0})
				}
				if r.URL.Query().Get("type") == "5" && (scenario == "channel-error" || scenario == "delayed-consumption") {
					items = append(items, map[string]any{"request_id": requestID, "prompt_tokens": 0, "completion_tokens": 0, "content": "invalid_api_key"})
				}
				json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"items": items}})
			}))
			defer evidenceServer.Close()
			policy, err := NewProviderURLPolicy("development", []string{"127.0.0.1"})
			if err != nil {
				t.Fatal(err)
			}
			evidence, err := NewNewAPIUsageEvidenceClient(evidenceServer.URL, "fixture-evidence-key", 1, policy)
			if err != nil {
				t.Fatal(err)
			}
			reconciler := NewUsageReconciler(tokenRepo, billingClient, evidence, time.Second)
			reconciler.runOnce(ctx)
			reconciler.runOnce(ctx)
			stored, err := tokenRepo.GetUsage(ctx, usage.ID)
			if err != nil {
				t.Fatal(err)
			}
			balance, err := billingRepo.GetWallet(ctx, wallet.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantStatus, wantPosted, wantReserved := "REVIEW", int64(100), int64(100)
			if scenario == "consumed" {
				wantStatus, wantPosted, wantReserved = "COMMITTED", 95, 0
			}
			if stored.Status != wantStatus || balance.PostedMicrocredits != wantPosted || balance.ReservedMicrocredits != wantReserved {
				t.Fatal("reconciliation violated its terminal or protected-funds invariant")
			}
			if scenario == "consumed" && (stored.ActualMicrocredits == nil || *stored.ActualMicrocredits != 5) {
				t.Fatal("historical pricing did not retain exact settlement")
			}
			if scenario == "delayed-consumption" {
				// The failed-channel log exists first; a later retried channel
				// publishes consumption for the same logical request ID.
				consumptionAvailable.Store(true)
				if _, err := tokenRepo.pool.Exec(ctx, "UPDATE usage_requests SET reconciliation_next_at=now()-interval '1 second' WHERE id=$1", usage.ID); err != nil {
					t.Fatal(err)
				}
				reconciler.runOnce(ctx)
				reconciler.runOnce(ctx)
				stored, err = tokenRepo.GetUsage(ctx, usage.ID)
				if err != nil {
					t.Fatal(err)
				}
				balance, err = billingRepo.GetWallet(ctx, wallet.ID)
				if err != nil {
					t.Fatal(err)
				}
				if stored.Status != "COMMITTED" || stored.ActualMicrocredits == nil || *stored.ActualMicrocredits != 5 || balance.PostedMicrocredits != 95 || balance.ReservedMicrocredits != 0 {
					t.Fatal("delayed consumption was released early or not settled exactly once")
				}
				var settlements int
				if err := tokenRepo.pool.QueryRow(ctx, "SELECT count(*) FROM outbox_events WHERE aggregate_id=$1 AND event_type='token.usage.settled.v1'", usage.ID).Scan(&settlements); err != nil || settlements != 1 {
					t.Fatal("delayed settlement did not publish exactly one terminal event")
				}
			}
			if scenario == "overbound" {
				var stopped *time.Time
				if err := tokenRepo.pool.QueryRow(ctx, `SELECT reconciliation_stopped_at FROM usage_requests WHERE id=$1`, usage.ID).Scan(&stopped); err != nil || stopped == nil {
					t.Fatal("overbound evidence did not retain manual review")
				}
			}
		})
	}
}

func TestPostgresHTTPReconciliationCompletesOnlyPersistedProxyReleaseIntent(t *testing.T) {
	for _, scenario := range []string{"billing-unavailable", "billing-response-unknown", "local-attempt-write-failed", "hold-record-missing"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			tokenRepo, usage := settlementFixture(t)
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
			wallet, _, err := billingRepo.EnsureWallet(ctx, "PERSONAL", usage.UserID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := billingRepo.Credit(ctx, wallet.ID, "fixture-release-reconciliation", 100, true); err != nil {
				t.Fatal(err)
			}
			hold, err := billingRepo.Reserve(ctx, usage.ID, wallet.ID, 100, nil, time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tokenRepo.pool.Exec(ctx, "UPDATE usage_requests SET hold_id=$2,price_version_id=NULL,wallet_id=$3 WHERE id=$1", usage.ID, hold.ID, wallet.ID); err != nil {
				t.Fatal(err)
			}
			if scenario == "hold-record-missing" {
				if _, err := tokenRepo.pool.Exec(ctx, "UPDATE usage_requests SET hold_id=NULL WHERE id=$1", usage.ID); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "local-attempt-write-failed" {
				if err := tokenRepo.RecordUpstreamRequest(ctx, usage.ID, "fixture-confirmed-rejection"); err != nil {
					t.Fatal(err)
				}
				if _, err := tokenRepo.pool.Exec(ctx, `CREATE FUNCTION fixture_fail_release_attempt() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture release record unavailable'; END $$; CREATE TRIGGER fixture_fail_release_attempt BEFORE UPDATE ON usage_attempts FOR EACH ROW EXECUTE FUNCTION fixture_fail_release_attempt()`); err != nil {
					t.Fatal(err)
				}
			}
			serviceToken := strings.Repeat("r", 32)
			billingHandler := billingdomain.NewService(billingRepo).Handler(serviceToken)
			var releaseCalls atomic.Int64
			billingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/internal/v1/holds/release" {
					payload, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error("release payload read failed")
						w.WriteHeader(400)
						return
					}
					var body struct{ Reason string }
					if json.Unmarshal(payload, &body) != nil || body.Reason != "upstream_rejected" {
						t.Error("release changed the original reason")
						w.WriteHeader(400)
						return
					}
					r.Body = io.NopCloser(strings.NewReader(string(payload)))
					first := releaseCalls.Add(1) == 1
					if first && (scenario == "billing-unavailable" || scenario == "hold-record-missing") {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					if first && scenario == "billing-response-unknown" {
						rec := httptest.NewRecorder()
						billingHandler.ServeHTTP(rec, r)
						if rec.Code != http.StatusOK {
							t.Error("fixture release did not commit before unknown response")
						}
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
				}
				billingHandler.ServeHTTP(w, r)
			}))
			defer billingServer.Close()
			billingClient, err := NewHTTPBillingClient(billingServer.URL, serviceToken)
			if err != nil {
				t.Fatal(err)
			}
			(&Service{repository: tokenRepo, billing: billingClient}).releaseUnused(ctx, usage.ID, hold.ID, "upstream_rejected")
			stored, err := tokenRepo.GetUsage(ctx, usage.ID)
			if err != nil || stored.Status != "REVIEW" {
				t.Fatal("failed proxy release did not retain REVIEW")
			}
			if scenario != "local-attempt-write-failed" {
				var started bool
				if err := tokenRepo.pool.QueryRow(ctx, "SELECT (usage_snapshot->>'upstream_started')::boolean FROM usage_requests WHERE id=$1", usage.ID).Scan(&started); err != nil || !started {
					t.Fatal("confirmed upstream response was misrecorded as never started")
				}
			}
			balance, err := billingRepo.GetWallet(ctx, wallet.ID)
			if err != nil {
				t.Fatal(err)
			}
			expectedReserved := int64(100)
			if scenario == "billing-response-unknown" || scenario == "local-attempt-write-failed" {
				expectedReserved = 0
			}
			if balance.PostedMicrocredits != 100 || balance.ReservedMicrocredits != expectedReserved {
				t.Fatal("release failure changed protected or credited funds")
			}
			if scenario == "local-attempt-write-failed" {
				if _, err := tokenRepo.pool.Exec(ctx, "DROP TRIGGER fixture_fail_release_attempt ON usage_attempts; DROP FUNCTION fixture_fail_release_attempt()"); err != nil {
					t.Fatal(err)
				}
			}
			reconciler := NewUsageReconciler(tokenRepo, billingClient, nil, time.Second)
			reconciler.runOnce(ctx)
			reconciler.runOnce(ctx)
			stored, err = tokenRepo.GetUsage(ctx, usage.ID)
			if err != nil {
				t.Fatal(err)
			}
			balance, err = billingRepo.GetWallet(ctx, wallet.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Status != "RELEASED" || stored.ErrorClass == nil || *stored.ErrorClass != "upstream_rejected" || balance.PostedMicrocredits != 100 || balance.ReservedMicrocredits != 0 {
				t.Fatal("durable known-release intent did not converge without New API evidence or pricing")
			}
			expectedCalls := int64(2)
			if scenario == "local-attempt-write-failed" {
				expectedCalls = 1
			}
			if releaseCalls.Load() != expectedCalls {
				t.Fatal("release retry was missing, duplicated, or repeated confirmed Billing release")
			}
			if scenario == "local-attempt-write-failed" {
				var state string
				if err := tokenRepo.pool.QueryRow(ctx, "SELECT status FROM usage_attempts WHERE usage_request_id=$1", usage.ID).Scan(&state); err != nil || state != "FAILED" {
					t.Fatal("confirmed release did not finish its attempt atomically")
				}
			}
		})
	}
}
