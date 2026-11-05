package token

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/google/uuid"
)

func TestPersonalZeroBudgetCancellationAndExpiryPostgres(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	r, ctx := retainedManagementTokenRepository(t)
	funds, _, pool := retainedManagementBillingRepository(t)
	user := uuid.New()
	wallet, _, err := funds.EnsureWallet(ctx, "PERSONAL", user)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = funds.Credit(ctx, wallet.ID, "zero-cancellation-control", 1000, false); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	var loseACK atomic.Bool
	handler := billing.NewService(funds).Handler("controlled-zero-hold-service")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/internal/v1/holds/release" {
			calls.Add(1)
			if loseACK.Swap(false) {
				receipt := httptest.NewRecorder()
				handler.ServeHTTP(receipt, request)
				if receipt.Code != 200 {
					t.Errorf("actual release failed before lost ACK: %d %s", receipt.Code, receipt.Body.String())
				}
				w.WriteHeader(503)
				return
			}
		}
		handler.ServeHTTP(w, request)
	}))
	defer server.Close()
	client, err := NewHTTPBillingClient(server.URL, "controlled-zero-hold-service")
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{repository: r, billing: client, cancellationBilling: client, cancellationStore: r}
	// Seed an already admitted canonical free request, before reservation or any
	// dispatch. The existing fixture's sealed envelope and persisted maximum are
	// changed together; this is controlled producer data, not a policy mutation.
	create := func(t *testing.T, state string, expired bool) (uuid.UUID, billing.Hold) {
		t.Helper()
		usage := retainedCancellationUsage(t, r, ctx, user, wallet.ID, nil, state)
		envelope, err := r.LoadDispatchPreparation(ctx, usage)
		if err != nil {
			t.Fatal(err)
		}
		envelope.MaximumCost = 0
		raw, err := json.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}
		sealed, err := r.secrets.Encrypt(raw, replayAAD(usage, "dispatch", 0))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = r.pool.Exec(ctx, `UPDATE usage_requests SET idempotency_key_digest=$2,request_fingerprint=$2,maximum_microcredits=0,status='RESERVED',settlement_status='HOLD_ACTIVE',cancel_status='NOT_REQUESTED',cancel_requested_at=NULL WHERE id=$1`, usage, replayDigest([]byte(usage.String()))); err != nil {
			t.Fatal(err)
		}
		if _, err = r.pool.Exec(ctx, `UPDATE usage_dispatch_preparations SET encrypted_envelope=$2,envelope_integrity=$3 WHERE usage_request_id=$1`, usage, sealed, replayDigest(sealed)); err != nil {
			t.Fatal(err)
		}
		hold, err := funds.Reserve(ctx, usage, wallet.ID, 0, nil, time.Now().UTC().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = r.pool.Exec(ctx, `UPDATE usage_requests SET hold_id=$2 WHERE id=$1`, usage, hold.ID); err != nil {
			t.Fatal(err)
		}
		if expired {
			if _, err = pool.Exec(ctx, `UPDATE usage_holds SET expires_at=clock_timestamp()-interval '1 minute' WHERE id=$1`, hold.ID); err != nil {
				t.Fatal(err)
			}
			if err = pool.QueryRow(ctx, `SELECT expires_at FROM usage_holds WHERE id=$1`, hold.ID).Scan(&hold.ExpiresAt); err != nil {
				t.Fatal(err)
			}
		}
		return usage, hold
	}
	assertFinancial := func(t *testing.T, hold uuid.UUID, status string) {
		t.Helper()
		var actual string
		var allocationCount, journalCount int
		if err := pool.QueryRow(ctx, `SELECT status,(SELECT count(*) FROM usage_hold_allocations WHERE hold_id=h.id),(SELECT count(*) FROM journal_entries) FROM usage_holds h WHERE id=$1`, hold).Scan(&actual, &allocationCount, &journalCount); err != nil || actual != status || allocationCount != 0 || journalCount != 1 {
			t.Fatal("financial facts", actual, allocationCount, journalCount, err)
		}
		got, err := funds.GetWallet(ctx, wallet.ID)
		if err != nil || got.PostedMicrocredits != 1000 || got.ReservedMicrocredits != 0 || got.AvailableMicrocredits != 1000 {
			t.Fatal("free request moved funds", got, err)
		}
	}
	t.Run("ordinary-user-cancel-releases-and-replays-zero-hold", func(t *testing.T) {
		usage, hold := create(t, "HOLD_RESERVED", false)
		_, pre, err := r.RequestCancellation(ctx, usage, user, strings.Repeat("c", 64), cancellationFingerprint(usage), "controlled-zero-price-v1")
		if err != nil || !pre {
			t.Fatal("actual cancellation fence", pre, err)
		}
		result, err := service.finishPreDispatchCancellation(ctx, usage)
		if err != nil || result.CancelStatus != "LOCAL_CONFIRMED" || result.ExecutionStatus != "CANCELLED_BEFORE_DISPATCH" || result.SettlementStatus != "RELEASED" {
			t.Fatal("zero cancel must converge", result, err)
		}
		assertFinancial(t, hold.ID, "RELEASED")
		var version int64
		if err = pool.QueryRow(ctx, `SELECT version FROM wallets WHERE id=$1`, wallet.ID).Scan(&version); err != nil {
			t.Fatal(err)
		}
		for n := 0; n < 2; n++ {
			if err = client.Release(ctx, usage, hold.ID, "same release retry"); err != nil {
				t.Fatal(err)
			}
		}
		var after int64
		if err = pool.QueryRow(ctx, `SELECT version FROM wallets WHERE id=$1`, wallet.ID).Scan(&after); err != nil || after != version {
			t.Fatal("replay advanced wallet", after, version, err)
		}
		assertFinancial(t, hold.ID, "RELEASED")
	})
	t.Run("expired-pre-dispatch-lost-release-ack-recovers-original-hold", func(t *testing.T) {
		usage, hold := create(t, "HOLD_RESERVED", true)
		loseACK.Store(true)
		worker := billing.NewExpiredHoldRecoveryWorker(funds, f04LocalExpiryClient{service}, time.Minute)
		if err := worker.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		current, err := r.GetUsage(ctx, usage)
		if err != nil || current.CancelStatus == "LOCAL_CONFIRMED" || current.ExecutionStatus != "REVIEW" {
			t.Fatal("lost ACK falsely confirmed", current, err)
		}
		assertFinancial(t, hold.ID, "RELEASED")
		var state string
		if err = pool.QueryRow(ctx, `SELECT state FROM usage_hold_expiry_recovery WHERE hold_id=$1`, hold.ID).Scan(&state); err != nil || state != "MANUAL_REVIEW" {
			t.Fatal(state, err)
		}
		if _, err = pool.Exec(ctx, `UPDATE usage_hold_expiry_recovery SET next_attempt_at=clock_timestamp() WHERE hold_id=$1`, hold.ID); err != nil {
			t.Fatal(err)
		}
		if err = worker.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		current, err = r.GetUsage(ctx, usage)
		if err != nil || current.CancelStatus != "LOCAL_CONFIRMED" || current.SettlementStatus != "RELEASED" {
			t.Fatal("expiry recovery did not converge", current, err)
		}
		if err = pool.QueryRow(ctx, `SELECT state FROM usage_hold_expiry_recovery WHERE hold_id=$1`, hold.ID).Scan(&state); err != nil || state != "COMPLETED" {
			t.Fatal(state, err)
		}
		var confirmed int
		if err = pool.QueryRow(ctx, `SELECT count(*) FROM usage_hold_expiry_evidence WHERE hold_id=$1 AND decision='CONFIRMED'`, hold.ID).Scan(&confirmed); err != nil || confirmed != 1 {
			t.Fatal("non-idempotent recovery", confirmed, err)
		}
		assertFinancial(t, hold.ID, "RELEASED")
	})
	t.Run("zero-budget-does-not-prove-undispatched-streaming-request", func(t *testing.T) {
		_, hold := create(t, "STREAMING", true)
		before := calls.Load()
		worker := billing.NewExpiredHoldRecoveryWorker(funds, f04LocalExpiryClient{service}, time.Minute)
		if err := worker.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if calls.Load() != before {
			t.Fatal("unproven free request dispatched release")
		}
		assertFinancial(t, hold.ID, "ACTIVE")
		var state string
		if err = pool.QueryRow(ctx, `SELECT state FROM usage_hold_expiry_recovery WHERE hold_id=$1`, hold.ID).Scan(&state); err != nil || state != "MANUAL_REVIEW" {
			t.Fatal(state, err)
		}
	})
}
