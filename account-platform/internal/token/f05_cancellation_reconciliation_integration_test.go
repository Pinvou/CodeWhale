package token

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/google/uuid"
)

// These scenarios use the existing isolated Token/Billing public-migration
// fixtures. They do not start a deployment or infer release from local state.
func TestF05CancellationRecoveryReconciliationOwnership(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	r, ctx := retainedManagementTokenRepository(t)
	funds, _, pool := retainedManagementBillingRepository(t)

	for _, committed := range []bool{false, true} {
		name := "release-failed-before-commit"
		if committed {
			name = "release-committed-ack-lost"
		}
		t.Run(name, func(t *testing.T) {
			user := uuid.New()
			wallet, _, err := funds.EnsureWallet(ctx, "PERSONAL", user)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = funds.Credit(ctx, wallet.ID, "ownership-"+user.String(), 1000, false); err != nil {
				t.Fatal(err)
			}
			id := retainedCancellationUsage(t, r, ctx, user, wallet.ID, nil, "HOLD_RESERVED")
			hold, err := funds.Reserve(ctx, id, wallet.ID, 100, nil, time.Now().Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = pool.Exec(ctx, `UPDATE usage_holds SET expires_at=clock_timestamp()-interval '1 minute' WHERE id=$1`, hold.ID); err != nil {
				t.Fatal(err)
			}
			if _, err = r.pool.Exec(ctx, `UPDATE usage_requests SET hold_id=$2,status='RESERVED',settlement_status='HOLD_ACTIVE',cancel_status='NOT_REQUESTED' WHERE id=$1`, id, hold.ID); err != nil {
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
						if committed {
							receipt := httptest.NewRecorder()
							handler.ServeHTTP(receipt, request)
							if receipt.Code != http.StatusOK {
								t.Error("real release failed before ACK injection", receipt.Code)
							}
						}
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
			service := &Service{repository: r, billing: client, cancellationBilling: client, cancellationStore: r}
			expiry := billing.NewExpiredHoldRecoveryWorker(funds, f04LocalExpiryClient{service}, time.Minute)
			if err = expiry.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			current, err := r.GetUsage(ctx, id)
			if err != nil || current.ExecutionStatus != "REVIEW" || current.CancelStatus != "CANCEL_REQUESTED" || current.ErrorClass == nil || *current.ErrorClass != "cancel_hold_recovery_unconfirmed" {
				t.Fatal("uncertain release did not preserve original recovery ownership", current, err)
			}
			var proofID uuid.UUID
			if err = r.pool.QueryRow(ctx, `SELECT id FROM usage_cancellation_evidence WHERE usage_request_id=$1 AND adapter_version='hold-expiry-v1'`, id).Scan(&proofID); err != nil {
				t.Fatal(err)
			}
			before := f05CancellationRecoveryFacts(t, r, ctx, id)
			evidence := &f05NoCancellationProviderLookup{t: t}
			NewUsageReconciler(r, client, evidence, time.Second).runOnce(ctx)
			if after := f05CancellationRecoveryFacts(t, r, ctx, id); after != before {
				// Continue to prove the actual saved-proof consequence on the old
				// implementation, rather than stopping at a field-only assertion.
				t.Error("settlement worker changed cancellation-owned facts")
			}
			if releases.Load() != 1 {
				t.Error("ordinary reconciliation called Billing release", releases.Load())
			}
			wallet, err = funds.GetWallet(ctx, wallet.ID)
			wantReserved := int64(100)
			if committed {
				wantReserved = 0
			}
			if err != nil || wallet.ReservedMicrocredits != wantReserved {
				t.Fatal("uncertainty changed actual money", wallet, err)
			}
			if _, err = pool.Exec(ctx, `UPDATE usage_hold_expiry_recovery SET next_attempt_at=clock_timestamp() WHERE hold_id=$1`, hold.ID); err != nil {
				t.Fatal(err)
			}
			if err = expiry.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			current, err = r.GetUsage(ctx, id)
			if err != nil || current.ExecutionStatus != "CANCELLED_BEFORE_DISPATCH" || current.CancelStatus != "LOCAL_CONFIRMED" || current.SettlementStatus != "RELEASED" {
				t.Fatal("same saved proof no longer completes verified release", current, err)
			}
			var sameProof uuid.UUID
			var state string
			if err = r.pool.QueryRow(ctx, `SELECT id FROM usage_cancellation_evidence WHERE usage_request_id=$1 AND adapter_version='hold-expiry-v1'`, id).Scan(&sameProof); err != nil || sameProof != proofID {
				t.Fatal("recovery changed proof identity", sameProof, err)
			}
			if err = pool.QueryRow(ctx, `SELECT state FROM usage_hold_expiry_recovery WHERE hold_id=$1`, hold.ID).Scan(&state); err != nil || state != "COMPLETED" {
				t.Fatal("Billing recovery did not complete", state, err)
			}
			wallet, err = funds.GetWallet(ctx, wallet.ID)
			if err != nil || wallet.ReservedMicrocredits != 0 || wallet.AvailableMicrocredits != 1000 || releases.Load() != 2 {
				t.Fatal("verified recovery money/release count", wallet, releases.Load(), err)
			}
			// An already completed recovery cannot release or reserve again.
			if err = expiry.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if releases.Load() != 2 {
				t.Fatal("completed recovery duplicated release", releases.Load())
			}
		})
	}

	t.Run("lost-reserve-ack-original-user-proof", func(t *testing.T) {
		user := uuid.New()
		wallet, _, err := funds.EnsureWallet(ctx, "PERSONAL", user)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = funds.Credit(ctx, wallet.ID, "lost-reserve-"+user.String(), 1000, false); err != nil {
			t.Fatal(err)
		}
		id := retainedCancellationUsage(t, r, ctx, user, wallet.ID, nil, "HOLD_RESERVING")
		envelope, err := r.LoadDispatchPreparation(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = pool.Exec(ctx, `INSERT INTO pricing_versions(id,model,input_per_million,output_per_million,cached_input_per_million,effective_from,created_at,scope_id,published_at) VALUES($1,'model',0,0,0,now(),now(),'model',now())`, envelope.Price.ID); err != nil {
			t.Fatal(err)
		}
		usage, err := r.GetUsage(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		// Reserve once using the exact retained immutable pricing/request facts;
		// deliberately lose its Token attachment, as in the existing regression.
		priceJSON, err := json.Marshal(envelope.Price)
		if err != nil {
			t.Fatal(err)
		}
		pricing := &billing.HoldPricingContext{ModelID: usage.Model, ProtocolCode: "CHAT", FormulaVersionID: envelope.Price.FormulaVersionID, PricingSnapshot: priceJSON, RequestSnapshot: usage.RequestSnapshot}
		hold, err := funds.ReservePriced(ctx, id, wallet.ID, 100, &envelope.Price.ID, time.Now().Add(time.Hour), pricing)
		if err != nil {
			t.Fatal(err)
		}
		if err = r.MarkReview(ctx, id, "cancel_hold_recovery_unconfirmed", map[string]any{"customer_hold_release_unconfirmed": true}); err != nil {
			t.Fatal(err)
		}
		if _, err = r.pool.Exec(ctx, `INSERT INTO usage_cancellation_evidence(id,usage_request_id,evidence_type,adapter_version,sanitized_evidence,occurred_at) VALUES($1,$2,'CANCEL_REQUESTED','controlled',jsonb_build_object('pre_dispatch',true),now())`, uuid.New(), id); err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(billing.NewService(funds).Handler("controlled-service"))
		defer server.Close()
		client, err := NewHTTPBillingClient(server.URL, "controlled-service")
		if err != nil {
			t.Fatal(err)
		}
		before := f05CancellationRecoveryFacts(t, r, ctx, id)
		NewUsageReconciler(r, client, &f05NoCancellationProviderLookup{t: t}, time.Second).runOnce(ctx)
		if after := f05CancellationRecoveryFacts(t, r, ctx, id); after != before {
			t.Error("worker erased unattached original-hold recovery")
		}
		service := &Service{billing: client, cancellationBilling: client, cancellationStore: r}
		result, err := service.finishPreDispatchCancellation(ctx, id)
		if err != nil || result.CancelStatus != "LOCAL_CONFIRMED" {
			t.Fatal("original reservation proof did not recover", result, err)
		}
		usage, err = r.GetUsage(ctx, id)
		if err != nil || usage.HoldID == nil || *usage.HoldID != hold.ID {
			t.Fatal("recovery did not attach original hold", usage, err)
		}
		wallet, err = funds.GetWallet(ctx, wallet.ID)
		if err != nil || wallet.ReservedMicrocredits != 0 || wallet.AvailableMicrocredits != 1000 {
			t.Fatal("original hold recovery changed money", wallet, err)
		}
	})

	t.Run("claimed-before-cancellation-stop-and-reschedule", func(t *testing.T) {
		id := retainedCancellationUsage(t, r, ctx, uuid.New(), uuid.New(), nil, "HOLD_RESERVING")
		if err := r.MarkReview(ctx, id, "ordinary-review-before-cancel", nil); err != nil {
			t.Fatal(err)
		}
		items, err := r.ClaimReconciliation(ctx, 50, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, item := range items {
			found = found || item.ID == id
		}
		if !found {
			t.Fatal("ordinary REVIEW was excluded before ownership changed")
		}
		if err = r.MarkReview(ctx, id, "cancel_hold_recovery_unconfirmed", map[string]any{"customer_hold_release_unconfirmed": true}); err != nil {
			t.Fatal(err)
		}
		before := f05CancellationRecoveryFacts(t, r, ctx, id)
		if err = r.StopReconciliation(ctx, id, "MANUAL_RECONCILIATION_REQUIRED"); !errors.Is(err, ErrForbidden) {
			t.Error("stale stop did not reject cancellation ownership", err)
		}
		if after := f05CancellationRecoveryFacts(t, r, ctx, id); after != before {
			t.Error("stale stop overwrote current cancellation/proof facts")
		}
		if err = r.RescheduleReconciliation(ctx, id, "STALE_UPSTREAM_LOOKUP", time.Now().Add(time.Hour)); !errors.Is(err, ErrForbidden) {
			t.Error("stale reschedule did not reject cancellation ownership", err)
		}
		if after := f05CancellationRecoveryFacts(t, r, ctx, id); after != before {
			t.Error("stale reschedule changed cancellation recovery facts")
		}
	})

	t.Run("ordinary-review-still-stops-without-upstream-facts", func(t *testing.T) {
		id := retainedCancellationUsage(t, r, ctx, uuid.New(), uuid.New(), nil, "HOLD_RESERVING")
		if err := r.MarkReview(ctx, id, "ordinary-upstream-uncertainty", nil); err != nil {
			t.Fatal(err)
		}
		NewUsageReconciler(r, &reconciliationBillingFake{}, &f05NoCancellationProviderLookup{t: t}, time.Second).runOnce(ctx)
		var stopped bool
		var reason string
		if err := r.pool.QueryRow(ctx, `SELECT reconciliation_stopped_at IS NOT NULL,error_class FROM usage_requests WHERE id=$1`, id).Scan(&stopped, &reason); err != nil || !stopped || reason != "automatic_reconciliation_exhausted" {
			t.Fatal("ordinary REVIEW lost existing fail-closed manual outcome", stopped, reason, err)
		}
	})
}

func f05CancellationRecoveryFacts(t *testing.T, r *PostgresRepository, ctx context.Context, id uuid.UUID) string {
	t.Helper()
	var facts string
	err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('usage',to_jsonb(u),'convergence',COALESCE((SELECT jsonb_agg(to_jsonb(e) ORDER BY e.id) FROM usage_convergence_evidence e WHERE e.usage_request_id=u.id),'[]'::jsonb),'cancellation',COALESCE((SELECT jsonb_agg(to_jsonb(e) ORDER BY e.id) FROM usage_cancellation_evidence e WHERE e.usage_request_id=u.id),'[]'::jsonb))::text FROM usage_requests u WHERE u.id=$1`, id).Scan(&facts)
	if err != nil {
		t.Fatal(err)
	}
	return facts
}

type f05NoCancellationProviderLookup struct{ t *testing.T }

func (*f05NoCancellationProviderLookup) ProviderID() string     { return "NEW_API" }
func (*f05NoCancellationProviderLookup) AdapterVersion() string { return "new-api-v1" }
func (e *f05NoCancellationProviderLookup) Lookup(context.Context, string) (UsageEvidence, error) {
	e.t.Error("pre-dispatch cancellation recovery performed provider lookup")
	return UsageEvidence{}, errors.New("unexpected provider lookup")
}
