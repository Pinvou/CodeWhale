package token

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/holdexpiry"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Public migrations and runtime repositories are real; the existing HTTP
// authorization/Billing fixtures supply controlled identities and one lost RPC.
// No provider or deployed issuer is exercised, and uncertainty never proves release.
func TestF05CancellationRecoveryClosurePublicPostgres(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	r, ctx := retainedManagementTokenRepository(t)
	funds, _, financePool := retainedManagementBillingRepository(t)
	for _, workerFirst := range []bool{false, true} {
		name := "user-cancellation-before-expiry-proof"
		if workerFirst {
			name = "worker-proof-before-first-user-cancellation"
		}
		t.Run(name, func(t *testing.T) {
			user := uuid.New()
			wallet, _, err := funds.EnsureWallet(ctx, "PERSONAL", user)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = funds.Credit(ctx, wallet.ID, "closure-"+user.String(), 1000, false); err != nil {
				t.Fatal(err)
			}
			id := retainedCancellationUsage(t, r, ctx, user, wallet.ID, nil, "HOLD_RESERVED")
			hold, err := funds.Reserve(ctx, id, wallet.ID, 100, nil, time.Now().Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = r.pool.Exec(ctx, `UPDATE usage_requests SET hold_id=$2,status='RESERVED',settlement_status='HOLD_ACTIVE',cancel_status='NOT_REQUESTED',cancel_requested_at=NULL WHERE id=$1`, id, hold.ID); err != nil {
				t.Fatal(err)
			}
			var failOnce atomic.Bool
			failOnce.Store(true)
			var releases atomic.Int64
			handler := billing.NewService(funds).Handler("controlled-service")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/internal/v1/holds/release" {
					releases.Add(1)
					if failOnce.Swap(false) {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
				}
				handler.ServeHTTP(w, request)
			}))
			defer server.Close()
			client, err := NewHTTPBillingClient(server.URL, "controlled-service")
			if err != nil {
				t.Fatal(err)
			}
			service, _, closeAuth := newModelUsageAuthorizationService(t, AuthorizationContext{UserID: user, SessionID: "controlled-session", ContextType: "PERSONAL"})
			defer closeAuth()
			service.repository, service.billing = r, client
			service.cancellationStore, service.cancellationBilling = r, client
			worker := billing.NewExpiredHoldRecoveryWorker(funds, f04LocalExpiryClient{service}, time.Minute)
			cancel := func(key string) *httptest.ResponseRecorder {
				request := httptest.NewRequest(http.MethodPost, "/v1/usage-requests/"+id.String()+"/cancel", nil).WithContext(ctx)
				request.Header.Set("Authorization", "Bearer controlled-token")
				request.Header.Set("Idempotency-Key", key)
				out := httptest.NewRecorder()
				service.Handler(successfulBillingVerify).ServeHTTP(out, request)
				return out
			}
			var originalProof uuid.UUID
			if workerFirst {
				if _, err = financePool.Exec(ctx, `UPDATE usage_holds SET expires_at=clock_timestamp()-interval '1 minute' WHERE id=$1`, hold.ID); err != nil {
					t.Fatal(err)
				}
				if err = worker.RunOnce(ctx); err != nil {
					t.Fatal(err)
				}
				if err = r.pool.QueryRow(ctx, `SELECT id FROM usage_cancellation_evidence WHERE usage_request_id=$1 AND adapter_version='hold-expiry-v1'`, id).Scan(&originalProof); err != nil {
					t.Fatal(err)
				}
				beforeUsage := f05CancellationRecoveryFacts(t, r, ctx, id)
				beforeFunds := f05CancellationClosureFundsFacts(t, financePool, ctx)
				out := cancel("controlled-first-user-cancellation")
				if out.Code != http.StatusOK {
					t.Error("first user command did not preserve pending worker recovery", out.Code, out.Body.String())
				}
				if after := f05CancellationRecoveryFacts(t, r, ctx, id); after != beforeUsage {
					t.Error("first user command overwrote cancellation-owned state or evidence")
				}
				if after := f05CancellationClosureFundsFacts(t, financePool, ctx); after != beforeFunds || releases.Load() != 1 {
					t.Error("pending user cancellation caused a funding effect")
				}
			} else {
				out := cancel("controlled-first-user-cancellation")
				if out.Code != http.StatusAccepted {
					t.Fatal("unconfirmed real user release was not retained", out.Code, out.Body.String())
				}
				var count int
				if err = r.pool.QueryRow(ctx, `SELECT count(*) FROM usage_cancellation_evidence WHERE usage_request_id=$1 AND adapter_version='hold-expiry-v1'`, id).Scan(&count); err != nil || count != 0 {
					t.Fatal("user cancellation unexpectedly installed an expiry proof", count, err)
				}
				if _, err = financePool.Exec(ctx, `UPDATE usage_holds SET expires_at=clock_timestamp()-interval '1 minute' WHERE id=$1`, hold.ID); err != nil {
					t.Fatal(err)
				}
			}
			current, err := r.GetUsage(ctx, id)
			if err != nil || current.ExecutionStatus != "REVIEW" || current.CancelStatus != "CANCEL_REQUESTED" || current.ErrorClass == nil || *current.ErrorClass != "cancel_hold_recovery_unconfirmed" {
				t.Error("uncertain cancellation lost its original recovery facts", current, err)
			}
			// Same and different keys reuse the persisted cancellation command;
			// neither retry can declare release or create another Billing request.
			beforeRetry := f05CancellationRecoveryFacts(t, r, ctx, id)
			for _, key := range []string{"controlled-first-user-cancellation", "controlled-second-user-cancellation"} {
				if out := cancel(key); out.Code != http.StatusOK {
					t.Error("pending command replay failed", out.Code, out.Body.String())
				}
			}
			if after := f05CancellationRecoveryFacts(t, r, ctx, id); after != beforeRetry || releases.Load() != 1 {
				t.Error("pending command replay changed cancellation evidence or released funds")
			}
			if _, err = financePool.Exec(ctx, `UPDATE usage_hold_expiry_recovery SET next_attempt_at=clock_timestamp() WHERE hold_id=$1`, hold.ID); err != nil {
				t.Fatal(err)
			}
			if err = worker.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			current, err = r.GetUsage(ctx, id)
			if err != nil || current.ExecutionStatus != "CANCELLED_BEFORE_DISPATCH" || current.CancelStatus != "LOCAL_CONFIRMED" || current.SettlementStatus != "RELEASED" {
				t.Fatal("real expiry worker could not close retained user cancellation", current, err)
			}
			var proof uuid.UUID
			if err = r.pool.QueryRow(ctx, `SELECT id FROM usage_cancellation_evidence WHERE usage_request_id=$1 AND adapter_version='hold-expiry-v1'`, id).Scan(&proof); err != nil || proof == uuid.Nil || workerFirst && proof != originalProof {
				t.Fatal("recovery lost or replaced its original proof", proof, err)
			}
			var state string
			if err = financePool.QueryRow(ctx, `SELECT state FROM usage_hold_expiry_recovery WHERE hold_id=$1`, hold.ID).Scan(&state); err != nil || state != "COMPLETED" {
				t.Fatal("Billing recovery is not complete", state, err)
			}
			wallet, err = funds.GetWallet(ctx, wallet.ID)
			if err != nil || wallet.ReservedMicrocredits != 0 || wallet.AvailableMicrocredits != 1000 || releases.Load() != 2 {
				t.Fatal("verified recovery did not restore the original funds exactly once", wallet, releases.Load(), err)
			}
			beforeTerminal := f05CancellationClosureFundsFacts(t, financePool, ctx)
			if err = worker.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if after := f05CancellationClosureFundsFacts(t, financePool, ctx); after != beforeTerminal || releases.Load() != 2 {
				t.Fatal("completed recovery repeated financial effects")
			}
		})
	}
	for _, scenario := range []string{"wrong-reason", "missing-local-proof", "wrong-local-proof", "wrong-hold", "wrong-wallet", "wrong-amount", "legacy", "no-attached-hold", "dispatch-intent"} {
		t.Run("reject-"+scenario, func(t *testing.T) {
			hold, wallet, user := uuid.New(), uuid.New(), uuid.New()
			id := retainedCancellationUsage(t, r, ctx, user, wallet, &hold, "HOLD_RESERVED")
			if _, err := r.pool.Exec(ctx, `UPDATE usage_requests SET cancel_status='NOT_REQUESTED',cancel_requested_at=NULL WHERE id=$1`, id); err != nil {
				t.Fatal(err)
			}
			if scenario == "dispatch-intent" {
				if _, err := r.pool.Exec(ctx, `INSERT INTO usage_dispatch_intents(usage_request_id,upstream_idempotency_key,created_at) VALUES($1,'controlled-dispatched',clock_timestamp())`, id); err != nil {
					t.Fatal(err)
				}
			}
			if scenario != "missing-local-proof" && scenario != "wrong-local-proof" {
				if _, local, err := r.RequestCancellation(ctx, id, user, digestString([]byte("negative-"+id.String())), cancellationFingerprint(id), "controlled-v1"); err != nil || !local {
					t.Fatal("local cancellation fixture failed", local, err)
				}
			} else {
				if _, err := r.pool.Exec(ctx, `UPDATE usage_requests SET cancel_status='CANCEL_REQUESTED' WHERE id=$1`, id); err != nil {
					t.Fatal(err)
				}
				if scenario == "wrong-local-proof" {
					if _, err := r.pool.Exec(ctx, `INSERT INTO usage_cancellation_evidence(id,usage_request_id,evidence_type,adapter_version,sanitized_evidence,occurred_at) VALUES($1,$2,'CANCEL_REQUESTED','controlled-v1','{"capability":"UNSUPPORTED","pre_dispatch":false}',clock_timestamp())`, uuid.New(), id); err != nil {
						t.Fatal(err)
					}
				}
			}
			reason := "cancel_hold_recovery_unconfirmed"
			if scenario == "wrong-reason" {
				reason = "ordinary-provider-uncertainty"
			}
			if err := r.MarkReview(ctx, id, reason, map[string]any{"customer_hold_release_unconfirmed": true}); err != nil {
				t.Fatal(err)
			}
			if scenario == "legacy" {
				if _, err := r.pool.Exec(ctx, `UPDATE usage_requests SET canonicalization_version=0 WHERE id=$1`, id); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "no-attached-hold" {
				if _, err := r.pool.Exec(ctx, `UPDATE usage_requests SET hold_id=NULL WHERE id=$1`, id); err != nil {
					t.Fatal(err)
				}
			}
			identity := holdexpiry.Identity{UsageRequestID: id, HoldID: hold, WalletID: wallet, MaxMicrocredits: 100, ExpiresAt: time.Now().UTC().Add(-time.Minute)}
			switch scenario {
			case "wrong-hold":
				identity.HoldID = uuid.New()
			case "wrong-wallet":
				identity.WalletID = uuid.New()
			case "wrong-amount":
				identity.MaxMicrocredits++
			}
			beforeUsage := f05CancellationRecoveryFacts(t, r, ctx, id)
			beforeFunds := f05CancellationClosureFundsFacts(t, financePool, ctx)
			if _, err := r.fenceExpiredHold(ctx, identity); !errors.Is(err, ErrForbidden) {
				t.Fatal("unproven cancellation accepted an expiry proof", scenario, err)
			}
			if after := f05CancellationRecoveryFacts(t, r, ctx, id); after != beforeUsage {
				t.Fatal("rejected proof changed retained cancellation facts", scenario)
			}
			if after := f05CancellationClosureFundsFacts(t, financePool, ctx); after != beforeFunds {
				t.Fatal("rejected proof changed financial facts", scenario)
			}
			if _, err := r.SyncManualReviewCases(ctx, 100); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM manual_review_cases WHERE domain='TOKEN' AND case_type='USAGE_RECONCILIATION_REVIEW' AND resource_id=$1`, id).Scan(&count); err != nil || reason == "cancel_hold_recovery_unconfirmed" && count != 1 || reason != "cancel_hold_recovery_unconfirmed" && count != 0 {
				t.Error("unconfirmed cancellation lacks a visible existing manual case", scenario, count, err)
			}
			if after := f05CancellationClosureFundsFacts(t, financePool, ctx); after != beforeFunds {
				t.Fatal("manual visibility was mistaken for financial closure")
			}
		})
	}
}

func f05CancellationClosureFundsFacts(t *testing.T, pool *pgxpool.Pool, ctx context.Context) string {
	t.Helper()
	var facts string
	if err := pool.QueryRow(ctx, `SELECT jsonb_build_object('wallets',(SELECT jsonb_agg(to_jsonb(w) ORDER BY w.id) FROM wallets w),'holds',(SELECT jsonb_agg(to_jsonb(h) ORDER BY h.id) FROM usage_holds h),'leases',(SELECT jsonb_agg(to_jsonb(e) ORDER BY e.id) FROM usage_hold_lease_events e),'journals',(SELECT jsonb_agg(to_jsonb(j) ORDER BY j.id) FROM journal_entries j),'postings',(SELECT jsonb_agg(to_jsonb(p) ORDER BY p.id) FROM ledger_postings p))::text`).Scan(&facts); err != nil {
		t.Fatal(err)
	}
	return facts
}
