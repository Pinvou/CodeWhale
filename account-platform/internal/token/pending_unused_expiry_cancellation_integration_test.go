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

// Simulate a Token process losing its callback after the real fence commits,
// before its release call. Billing's own persisted recovery intent remains real.
type pendingExpiryFenceLossClient struct {
	r       *PostgresRepository
	service *Service
	fenced  bool
}

func (c *pendingExpiryFenceLossClient) FenceExpiredHold(ctx context.Context, identity billing.ExpiredHoldIdentity) (billing.ExpiredHoldProof, error) {
	if !c.fenced {
		if _, err := c.r.fenceExpiredHold(ctx, identity); err != nil {
			return billing.ExpiredHoldProof{}, err
		}
		c.fenced = true
		return billing.ExpiredHoldProof{}, errors.New("controlled crash after committed expiry fence")
	}
	return c.service.recoverExpiredHold(ctx, identity)
}

func pendingExpiryObservation(t *testing.T, f modelExpiryObservationFixture, layout string, claim bool) (UsageRequest, uuid.UUID, *billing.ExpiredHoldRecoveryWorker) {
	t.Helper()
	u, target := f.preparedExpiry(t, layout)
	if claim {
		targets, err := f.r.claimEmergencyCancellations(f.ctx, 1)
		if err != nil || len(targets) != 1 || targets[0].ID != target {
			t.Fatal("claim before original expiry fence", targets, err)
		}
	}
	proof := &pendingExpiryFenceLossClient{r: f.r, service: f.service}
	expiry := billing.NewExpiredHoldRecoveryWorker(f.funds, proof, time.Minute)
	if err := expiry.RunOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	fenced, err := f.r.GetUsage(f.ctx, u.ID)
	if err != nil || !proof.fenced || fenced.ExecutionStatus != "HOLD_RESERVED" || fenced.CancelStatus != "CANCEL_REQUESTED" {
		t.Fatal("real pending expiry fence not reached", err)
	}
	return fenced, target, expiry
}

func TestPendingUnusedExpiryCancellationOwnerPostgres(t *testing.T) {
	f := newModelExpiryObservationFixture(t)
	for _, sample := range []struct {
		name, layout                   string
		model, user, lostACK, fairness bool
	}{
		{name: "model-claim-race", layout: "intent", model: true},
		{name: "user-first-and-retry", layout: "intent", user: true},
		{name: "model-after-user-command", layout: "intent", model: true, user: true},
		{name: "model-review-after-lost-release-ack", layout: "intent", model: true, user: true, lostACK: true},
		{name: "prior-user-stop", layout: "user-intent", model: true},
		{name: "no-intent-control", layout: "hold", model: true},
		{name: "waiting-owner-does-not-starve-later-target", layout: "intent", model: true, fairness: true},
	} {
		t.Run(sample.name, func(t *testing.T) {
			protected := sample.layout != "hold"
			u, target, expiry := pendingExpiryObservation(t, f, sample.layout, sample.model && protected)
			worker := NewEmergencyModelCancellationWorker(f.r, f.service, time.Second)
			facts := func() string {
				t.Helper()
				var value string
				if err := f.r.pool.QueryRow(f.ctx, `SELECT to_jsonb(request)::text FROM usage_requests request WHERE id=$1`, u.ID).Scan(&value); err != nil {
					t.Fatal(err)
				}
				return value
			}
			before := facts()
			denyModel := func() {
				t.Helper()
				if _, _, err := f.r.requestEmergencyCancellation(f.ctx, target, ProviderCancelUnsupported, f.service.upstream.AdapterVersion()); !errors.Is(err, ErrForbidden) {
					t.Fatal("model overwrote the original pending proof", err)
				}
				if err := f.r.retryEmergencyCancellation(f.ctx, target, ErrForbidden); err != nil {
					t.Fatal(err)
				}
			}
			if sample.model && protected && !sample.lostACK {
				denyModel()
				if facts() != before {
					t.Fatal("locked model recheck changed original facts")
				}
			}
			if sample.user {
				key := digestString([]byte("pending-expiry-user-" + u.ID.String()))
				for range 2 {
					result, pre, err := f.r.RequestCancellation(f.ctx, u.ID, u.UserID, key, cancellationFingerprint(u.ID), f.service.upstream.AdapterVersion())
					if err != nil || !pre || result.CancelStatus != "CANCEL_REQUESTED" {
						t.Fatal("existing user finish became unavailable", pre, err)
					}
					if facts() != before {
						t.Fatal("user command replaced original expiry facts")
					}
					if sample.model && !sample.lostACK {
						if err := worker.runOnce(f.ctx); err != nil {
							t.Fatal(err)
						}
					}
				}
				var commands int
				if err := f.r.pool.QueryRow(f.ctx, `SELECT count(*) FROM usage_cancel_commands WHERE usage_request_id=$1`, u.ID).Scan(&commands); err != nil || commands != 1 {
					t.Fatal("user command did not remain unique", commands, err)
				}
				if sample.lostACK {
					var lost atomic.Bool
					handler := billing.NewService(f.funds).Handler("controlled-pending-release")
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/internal/v1/holds/release" && !lost.Swap(true) {
							receipt := httptest.NewRecorder()
							handler.ServeHTTP(receipt, r)
							if receipt.Code != 200 {
								t.Error("real release before lost ACK", receipt.Code)
							}
							w.WriteHeader(http.StatusServiceUnavailable)
							return
						}
						handler.ServeHTTP(w, r)
					}))
					defer server.Close()
					client, err := NewHTTPBillingClient(server.URL, "controlled-pending-release")
					if err != nil {
						t.Fatal(err)
					}
					originalBilling, originalCancellation := f.service.billing, f.service.cancellationBilling
					f.service.billing, f.service.cancellationBilling = client, client
					defer func() { f.service.billing, f.service.cancellationBilling = originalBilling, originalCancellation }()
				}
				_, err := f.service.finishPreDispatchCancellation(f.ctx, u.ID)
				if sample.lostACK {
					current, e := f.r.GetUsage(f.ctx, u.ID)
					if err == nil || e != nil || current.ExecutionStatus != "REVIEW" || current.CancellationOrigin != u.CancellationOrigin {
						t.Fatal("lost release did not retain the original owner", err, e)
					}
					denyModel()
				} else if err != nil {
					t.Fatal(err)
				}
			}
			if sample.fairness {
				later, laterTarget := f.preparedExpiry(t, "hold")
				worker.batchSize = 1
				if err := worker.runOnce(f.ctx); err != nil {
					t.Fatal(err)
				}
				var state string
				if err := f.r.pool.QueryRow(f.ctx, `SELECT status FROM model_emergency_cancellations WHERE id=$1`, laterTarget).Scan(&state); err != nil || state != "CONVERGED" {
					t.Fatal("pending expiry owner starved a later runnable target", state, err)
				}
				wallet, err := f.funds.GetWallet(f.ctx, later.WalletID)
				if err != nil || wallet.ReservedMicrocredits != 0 {
					t.Fatal("later target did not release its own hold", err)
				}
			}
			if err := worker.runOnce(f.ctx); err != nil {
				t.Fatal(err)
			}
			if protected && !sample.user {
				wallet, err := f.funds.GetWallet(f.ctx, u.WalletID)
				if err != nil || wallet.ReservedMicrocredits != u.MaximumMicrocredits || facts() != before {
					t.Fatal("model claimed expiry-owned funds or facts", err)
				}
			}
			if _, err := f.bp.Exec(f.ctx, `UPDATE usage_hold_expiry_recovery SET next_attempt_at=clock_timestamp() WHERE hold_id=$1`, *u.HoldID); err != nil {
				t.Fatal(err)
			}
			if err := expiry.RunOnce(f.ctx); err != nil {
				t.Fatal(err)
			}
			var state string
			if err := f.bp.QueryRow(f.ctx, `SELECT state FROM usage_hold_expiry_recovery WHERE hold_id=$1`, *u.HoldID).Scan(&state); err != nil || state != "COMPLETED" {
				t.Fatal("original pending expiry did not complete", state, err)
			}
			for range 2 {
				if err := worker.runOnce(f.ctx); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.r.pool.QueryRow(f.ctx, `SELECT status FROM model_emergency_cancellations WHERE id=$1`, target).Scan(&state); err != nil || state != "CONVERGED" {
				t.Fatal("model did not observe the completed expiry", state, err)
			}
			wallet, err := f.funds.GetWallet(f.ctx, u.WalletID)
			if err != nil || wallet.ReservedMicrocredits != 0 || wallet.PostedMicrocredits != 1000 || wallet.AvailableMicrocredits != 1000 {
				t.Fatal("original wallet did not retain exact released funds", wallet, err)
			}
		})
	}
}

func TestPendingUnusedExpiryLegacyCancellationFallbackPostgres(t *testing.T) {
	f := newModelExpiryObservationFixture(t)
	for _, origin := range []string{"model-origin", "user-local-pre-dispatch"} {
		t.Run(origin, func(t *testing.T) {
			u, target, expiry := pendingExpiryObservation(t, f, "intent", false)
			var proofBefore string
			if err := f.r.pool.QueryRow(f.ctx, `SELECT sanitized_evidence::text FROM usage_cancellation_evidence WHERE usage_request_id=$1 AND adapter_version='hold-expiry-v1'`, u.ID).Scan(&proofBefore); err != nil {
				t.Fatal(err)
			}
			// Historical fixture: reproduce ba0ccf74's requestCancellation /
			// requestEmergencyCancellation writes after the committed expiry fence,
			// before finishPreDispatchCancellation. Current public writers must no
			// longer create these polluted layouts. Keep every immutable audit row
			// and all triggers; this tests fallback, not historical proof repair.
			tx, err := f.r.pool.Begin(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(f.ctx)
			at := f.r.now()
			adapter := f.service.upstream.AdapterVersion()
			if origin == "model-origin" {
				if _, err := tx.Exec(f.ctx, `UPDATE usage_requests SET cancel_status='CANCEL_REQUESTED',delivery_status='USER_STOPPED',cancellation_origin='MODEL_EMERGENCY',cancel_requested_at=COALESCE(cancel_requested_at,$2),user_stopped_at=COALESCE(user_stopped_at,$2),cancel_reconciliation_deadline=$2+make_interval(secs=>cancel_reconciliation_seconds),updated_at=$2 WHERE id=$1`, u.ID, at); err != nil {
					t.Fatal(err)
				}
				snapshot := map[string]any{"origin": "MODEL_EMERGENCY", "capability": ProviderCancelUnsupported, "pre_dispatch": true, "already_terminal": false, "provider_success_inferred": false}
				if _, err := tx.Exec(f.ctx, `INSERT INTO usage_cancellation_evidence(id,usage_request_id,evidence_type,adapter_version,sanitized_evidence,occurred_at) VALUES($1,$2,'MODEL_EMERGENCY_REQUESTED',$3,$4,$5)`, uuid.New(), u.ID, adapter, snapshot, at); err != nil {
					t.Fatal(err)
				}
				if err := appendUsageConvergenceEvidenceTx(f.ctx, tx, u.ID, "CANCELLATION", "GATEWAY", "RECORDED", "model-emergency-requested", snapshot, at); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(f.ctx, `INSERT INTO model_emergency_cancellation_evidence(id,emergency_cancellation_id,evidence_type,snapshot,created_at) VALUES($1,$2,'REQUESTED',$3,$4)`, uuid.New(), target, snapshot, at); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := tx.Exec(f.ctx, `INSERT INTO usage_cancel_commands(id,usage_request_id,user_id,idempotency_key_digest,request_fingerprint,cancellation_version,created_at) VALUES($1,$2,$3,$4,$5,1,$6)`, uuid.New(), u.ID, u.UserID, digestString([]byte("historical-pending-user-"+u.ID.String())), cancellationFingerprint(u.ID), at); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(f.ctx, `UPDATE usage_requests SET cancel_status='CANCEL_REQUESTED',delivery_status='USER_STOPPED',cancellation_origin='USER',cancel_requested_at=$2,user_stopped_at=$2,cancel_reconciliation_deadline=$2::timestamptz+make_interval(secs=>cancel_reconciliation_seconds),updated_at=$2 WHERE id=$1`, u.ID, at); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(f.ctx, `INSERT INTO usage_cancellation_evidence(id,usage_request_id,evidence_type,adapter_version,sanitized_evidence,occurred_at) VALUES($1,$2,'CANCEL_REQUESTED',$3,$4,$5)`, uuid.New(), u.ID, adapter, map[string]any{"capability": "LOCAL_PRE_DISPATCH", "pre_dispatch": true}, at); err != nil {
					t.Fatal(err)
				}
				if err := appendUsageConvergenceEvidenceTx(f.ctx, tx, u.ID, "CANCELLATION", "GATEWAY", "RECORDED", "requested:CANCEL_REQUESTED", map[string]any{"cancel_status": "CANCEL_REQUESTED", "capability": "LOCAL_PRE_DISPATCH", "pre_dispatch": true, "provider_success_inferred": false}, at); err != nil {
					t.Fatal(err)
				}
			}
			pending, err := pendingUnusedExpiryFenceTx(f.ctx, tx, u.ID)
			if err != nil || pending {
				t.Fatal("historical takeover must retain its existing fallback", pending, err)
			}
			if err := tx.Commit(f.ctx); err != nil {
				t.Fatal(err)
			}
			worker := NewEmergencyModelCancellationWorker(f.r, f.service, time.Second)
			for range 2 {
				if err := worker.runOnce(f.ctx); err != nil {
					t.Fatal(err)
				}
			}
			var state string
			if err := f.r.pool.QueryRow(f.ctx, `SELECT status FROM model_emergency_cancellations WHERE id=$1`, target).Scan(&state); err != nil || state != "CONVERGED" {
				t.Fatal("historical MODEL fallback did not converge", state, err)
			}
			current, err := f.r.GetUsage(f.ctx, u.ID)
			if err != nil || current.ExecutionStatus != "CANCELLED_BEFORE_DISPATCH" || current.CancelStatus != "LOCAL_CONFIRMED" || current.SettlementStatus != "RELEASED" || current.HoldID == nil || *current.HoldID != *u.HoldID {
				t.Fatal("fallback did not confirm the original hold", current, err)
			}
			wallet, err := f.funds.GetWallet(f.ctx, u.WalletID)
			if err != nil || wallet.ReservedMicrocredits != 0 || wallet.PostedMicrocredits != 1000 || wallet.AvailableMicrocredits != 1000 {
				t.Fatal("historical fallback did not restore exact original funds", wallet, err)
			}
			before := f05CancellationClosureFundsFacts(t, f.bp, f.ctx)
			if _, err := f.bp.Exec(f.ctx, `UPDATE usage_hold_expiry_recovery SET next_attempt_at=clock_timestamp() WHERE hold_id=$1`, *u.HoldID); err != nil {
				t.Fatal(err)
			}
			if err := expiry.RunOnce(f.ctx); err != nil {
				t.Fatal(err)
			}
			if err := f.bp.QueryRow(f.ctx, `SELECT state FROM usage_hold_expiry_recovery WHERE hold_id=$1`, *u.HoldID).Scan(&state); err != nil || state != "MANUAL_REVIEW" {
				t.Fatal("historical pollution must not be silently repaired", state, err)
			}
			var proofAfter string
			if err := f.r.pool.QueryRow(f.ctx, `SELECT sanitized_evidence::text FROM usage_cancellation_evidence WHERE usage_request_id=$1 AND adapter_version='hold-expiry-v1'`, u.ID).Scan(&proofAfter); err != nil || proofAfter != proofBefore {
				t.Fatal("original expiry identity changed", err)
			}
			if after := f05CancellationClosureFundsFacts(t, f.bp, f.ctx); after != before {
				t.Fatal("historical proof replay repeated financial effects")
			}
		})
	}
}

func TestPendingUnusedExpiryRejectsInvalidProofTimestampPostgres(t *testing.T) {
	f := newModelExpiryObservationFixture(t)
	for _, sample := range []struct {
		name, expiresAt                        string
		additionalProof, wrongHold, validFirst bool
	}{
		{name: "infinity", expiresAt: "infinity"},
		{name: "tomorrow", expiresAt: "tomorrow"},
		{name: "zero-time", expiresAt: "0001-01-01T00:00:00Z"},
		{name: "invalid-calendar", expiresAt: "2026-02-30T00:00:00Z"},
		{name: "bad-first-valid-later", expiresAt: "infinity", additionalProof: true},
		{name: "wrong-identity-first-valid-later", wrongHold: true, additionalProof: true},
		{name: "valid-first-later-bad", validFirst: true, additionalProof: true},
	} {
		t.Run(sample.name, func(t *testing.T) {
			u, target := f.preparedExpiry(t, "intent")
			identity := billing.ExpiredHoldIdentity{UsageRequestID: u.ID, HoldID: *u.HoldID, WalletID: u.WalletID, MaxMicrocredits: u.MaximumMicrocredits}
			if err := f.bp.QueryRow(f.ctx, `SELECT expires_at FROM usage_holds WHERE id=$1`, *u.HoldID).Scan(&identity.ExpiresAt); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(identity)
			if err != nil {
				t.Fatal(err)
			}
			good := append([]byte(nil), raw...)
			var fields map[string]any
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			if sample.expiresAt != "" {
				fields["expires_at"] = sample.expiresAt
			}
			if sample.wrongHold {
				fields["hold_id"] = uuid.New().String()
			}
			raw, err = json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			var parsed billing.ExpiredHoldIdentity
			if err := json.Unmarshal(raw, &parsed); !sample.validFirst && err == nil && parsed.Valid() && parsed.Matches(identity) {
				t.Fatal("negative fixture unexpectedly has the valid original Go expiry identity")
			}
			// Historical/corrupt negative fixture, not a normal public fence
			// writer: append a bad timestamp/identity (or a valid-first ordering
			// control) to a pending unused intent. Preserve all triggers/audit rows.
			tx, err := f.r.pool.Begin(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(f.ctx)
			at := f.r.now()
			if _, err := tx.Exec(f.ctx, `UPDATE usage_requests SET execution_status='HOLD_RESERVED',cancel_status='CANCEL_REQUESTED',cancel_requested_at=$2 WHERE id=$1`, u.ID, at); err != nil {
				t.Fatal(err)
			}
			proofID := uuid.New()
			if _, err := tx.Exec(f.ctx, `INSERT INTO usage_cancellation_evidence(id,usage_request_id,evidence_type,adapter_version,sanitized_evidence,occurred_at) VALUES($1,$2,'CANCEL_REQUESTED','hold-expiry-v1',$3,$4)`, proofID, u.ID, json.RawMessage(raw), at); err != nil {
				t.Fatal(err)
			}
			if sample.additionalProof {
				later := good
				if sample.validFirst {
					fields["expires_at"] = "infinity"
					later, err = json.Marshal(fields)
					if err != nil {
						t.Fatal(err)
					}
				}
				if _, err := tx.Exec(f.ctx, `INSERT INTO usage_cancellation_evidence(id,usage_request_id,evidence_type,adapter_version,sanitized_evidence,occurred_at) VALUES($1,$2,'CANCEL_REQUESTED','hold-expiry-v1',$3,$4)`, uuid.New(), u.ID, json.RawMessage(later), at.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			pending, err := pendingUnusedExpiryFenceTx(f.ctx, tx, u.ID)
			if err != nil || pending != sample.validFirst {
				t.Fatal("protection must classify the earliest expiry proof without SQL errors", pending, err)
			}
			if sample.validFirst {
				// Rollback-only selection control. This helper result does not
				// assert that the full validator accepts every later corrupt row.
				return
			}
			if err := tx.Commit(f.ctx); err != nil {
				t.Fatal(err)
			}
			before := f05CancellationClosureFundsFacts(t, f.bp, f.ctx)
			if _, err := f.r.fenceExpiredHold(f.ctx, identity); !errors.Is(err, ErrForbidden) {
				t.Fatal("bad historical proof must remain rejected by original expiry recovery", err)
			}
			if after := f05CancellationClosureFundsFacts(t, f.bp, f.ctx); after != before {
				t.Fatal("rejected corrupt expiry changed financial facts")
			}
			worker := NewEmergencyModelCancellationWorker(f.r, f.service, time.Second)
			for range 2 {
				if err := worker.runOnce(f.ctx); err != nil {
					t.Fatal(err)
				}
			}
			var state string
			if err := f.r.pool.QueryRow(f.ctx, `SELECT status FROM model_emergency_cancellations WHERE id=$1`, target).Scan(&state); err != nil || state != "CONVERGED" {
				t.Fatal("bad proof prevented original MODEL fallback", state, err)
			}
			current, err := f.r.GetUsage(f.ctx, u.ID)
			if err != nil || current.ExecutionStatus != "CANCELLED_BEFORE_DISPATCH" || current.CancelStatus != "LOCAL_CONFIRMED" || current.SettlementStatus != "RELEASED" || current.HoldID == nil || *current.HoldID != *u.HoldID {
				t.Fatal("bad-proof fallback did not confirm the original hold", current, err)
			}
			wallet, err := f.funds.GetWallet(f.ctx, u.WalletID)
			if err != nil || wallet.ReservedMicrocredits != 0 || wallet.PostedMicrocredits != 1000 || wallet.AvailableMicrocredits != 1000 {
				t.Fatal("bad-proof fallback did not restore exact original funds", wallet, err)
			}
			var saved []byte
			if err := f.r.pool.QueryRow(f.ctx, `SELECT sanitized_evidence FROM usage_cancellation_evidence WHERE id=$1`, proofID).Scan(&saved); err != nil || !expiryJSONEqual(saved, raw) {
				t.Fatal("fallback rewrote the corrupt historical proof", err)
			}
		})
	}
}
