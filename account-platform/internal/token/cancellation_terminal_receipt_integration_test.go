package token

import (
	"context"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/google/uuid"
)

// Schedule the user's first command after the real confirmation commits and
// before its caller receives it. Neither confirmation nor cancellation is mocked.
type cancellationConfirmationBoundary struct {
	*PostgresRepository
	afterConfirmation func(context.Context, uuid.UUID)
}

func (r *cancellationConfirmationBoundary) ConfirmPreDispatchCancellation(ctx context.Context, id, holdID uuid.UUID) (CancellationResult, error) {
	result, err := r.PostgresRepository.ConfirmPreDispatchCancellation(ctx, id, holdID)
	if err == nil {
		r.afterConfirmation(ctx, id)
	}
	return result, err
}

func TestCancellationFirstUserPreservesConfirmedReceiptPostgres(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	r, ctx := retainedManagementTokenRepository(t)
	funds, _, bp := retainedManagementBillingRepository(t)
	server := httptest.NewServer(billing.NewService(funds).Handler("controlled-terminal-receipt"))
	defer server.Close()
	client, err := NewHTTPBillingClient(server.URL, "controlled-terminal-receipt")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"model-before-attempt", "expiry-proof-ack-replay"} {
		t.Run(mode, func(t *testing.T) {
			u, envelope, hold := jR6UnboundSeed(t, r, funds, bp, client, ctx)
			// This is the original dispatch producer's committed ACK boundary,
			// before it promotes to a dispatch intent.
			if err := r.persistAcknowledgedHold(ctx, u.ID, hold.ID, envelope.Price.ID); err != nil {
				t.Fatal(err)
			}
			// Controlled policy metadata; the real worker derives its targets and
			// REQUESTED/ATTEMPTED evidence. This does not exercise Account approval.
			if mode == "model-before-attempt" {
				if _, err := r.pool.Exec(ctx, `INSERT INTO model_emergency_cancellation_policies(id,catalog_version,model_id,effective_at,requested_by,reason,created_at) VALUES($1,$2,$3,$4,$5,'controlled terminal receipt',now())`, uuid.New(), *u.CatalogVersion, *u.ModelID, time.Now().UTC(), u.UserID); err != nil {
					t.Fatal(err)
				}
			}
			var called atomic.Bool
			store := &cancellationConfirmationBoundary{PostgresRepository: r}
			store.afterConfirmation = func(ctx context.Context, id uuid.UUID) {
				if id != u.ID || called.Swap(true) {
					return
				}
				var before, after string
				const facts = `SELECT to_jsonb(request)::text FROM usage_requests request WHERE id=$1`
				if err := r.pool.QueryRow(ctx, facts, id).Scan(&before); err != nil {
					t.Fatal(err)
				}
				for range 2 {
					result, pre, err := r.RequestCancellation(ctx, id, u.UserID, digestString([]byte("terminal-user-"+id.String())), cancellationFingerprint(id), "controlled-v1")
					if err != nil || pre || result.CancelStatus != "LOCAL_CONFIRMED" || result.SettlementStatus != "RELEASED" {
						t.Error("first user command or replay lost confirmed receipt", result, pre, err)
					}
				}
				if err := r.pool.QueryRow(ctx, facts, id).Scan(&after); err != nil || before != after {
					t.Error("first user command changed the original confirmed usage facts", err)
				}
				var commands int
				if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM usage_cancel_commands WHERE usage_request_id=$1`, id).Scan(&commands); err != nil || commands != 1 {
					t.Error("first command was not recorded exactly once", commands, err)
				}
			}
			adapter := NewNewAPIGatewayAdapter()
			service := &Service{repository: r, billing: client, cancellationBilling: client, cancellationStore: store, cancellationAdapter: adapter, upstream: adapter}
			worker := NewEmergencyModelCancellationWorker(r, service, time.Second)
			var expiry *billing.ExpiredHoldRecoveryWorker
			if mode == "model-before-attempt" {
				if err := worker.runOnce(ctx); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := bp.Exec(ctx, `UPDATE usage_holds SET expires_at=clock_timestamp()-interval '1 minute' WHERE id=$1`, hold.ID); err != nil {
					t.Fatal(err)
				}
				expiry = billing.NewExpiredHoldRecoveryWorker(funds, &emergencyLostExpiryProofClient{service: service}, time.Minute)
				if err := expiry.RunOnce(ctx); err != nil {
					t.Fatal(err)
				}
				var state string
				if err := bp.QueryRow(ctx, `SELECT state FROM usage_hold_expiry_recovery WHERE hold_id=$1`, hold.ID).Scan(&state); err != nil || state != "MANUAL_REVIEW" {
					t.Fatal("lost proof response did not retain an uncertain result", state, err)
				}
				if _, err := bp.Exec(ctx, `UPDATE usage_hold_expiry_recovery SET next_attempt_at=clock_timestamp() WHERE hold_id=$1`, hold.ID); err != nil {
					t.Fatal(err)
				}
				if err := expiry.RunOnce(ctx); err != nil {
					t.Fatal(err)
				}
				if err := bp.QueryRow(ctx, `SELECT state FROM usage_hold_expiry_recovery WHERE hold_id=$1`, hold.ID).Scan(&state); err != nil || state != "COMPLETED" {
					t.Fatal("first user command prevented the original proof from completing", state, err)
				}
			}
			if !called.Load() {
				t.Fatal("confirmation/user interleaving was not reached")
			}
			before := f05CancellationClosureFundsFacts(t, bp, ctx)
			for range 2 {
				var err error
				if expiry != nil {
					err = expiry.RunOnce(ctx)
				} else {
					err = worker.runOnce(ctx)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if expiry == nil {
				var target string
				if err := r.pool.QueryRow(ctx, `SELECT status FROM model_emergency_cancellations WHERE usage_request_id=$1`, u.ID).Scan(&target); err != nil || target != "CONVERGED" {
					t.Fatal("confirmed cancellation did not converge", target, err)
				}
			}
			if after := f05CancellationClosureFundsFacts(t, bp, ctx); after != before {
				t.Fatal("terminal observation repeated financial effects")
			}
			wallet, err := funds.GetWallet(ctx, u.WalletID)
			if err != nil || wallet.ReservedMicrocredits != 0 || wallet.PostedMicrocredits != 1000 || wallet.AvailableMicrocredits != 1000 {
				t.Fatal("confirmed receipt lost exact wallet funds", wallet, err)
			}
		})
	}
}
