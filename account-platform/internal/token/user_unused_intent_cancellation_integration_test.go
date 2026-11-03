package token

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
)

func TestUserUnusedIntentCancellationOwnerPostgres(t *testing.T) {
	for _, name := range []string{"before-claim", "after-claim", "waiting-owner-does-not-starve-later-target", "acquired-intent-control", "model-first-control"} {
		t.Run(name, func(t *testing.T) {
			f := newModelExpiryObservationFixture(t)
			layout := "user-intent"
			if name == "after-claim" || name == "acquired-intent-control" || name == "model-first-control" {
				layout = "intent"
			}
			u, target := f.preparedExpiry(t, layout)
			claim := EmergencyCancellationTarget{ID: target}
			if name == "model-first-control" || name == "acquired-intent-control" || name == "after-claim" {
				claim = claimOneEmergencyCancellation(t, f.r, f.ctx, target)
			}
			if name == "model-first-control" {
				if _, _, err := f.r.requestEmergencyCancellation(f.ctx, claim, ProviderCancelUnsupported, f.service.upstream.AdapterVersion()); err != nil {
					t.Fatal("original MODEL request", err)
				}
			}
			if name == "acquired-intent-control" {
				envelope, acquired, err := f.r.AcquireDispatch(f.ctx, u.ID, "controlled-acquired-worker", time.Minute)
				if err != nil || !acquired {
					t.Fatal("real dispatch acquisition", acquired, err)
				}
				clear(envelope.Credential)
				clear(envelope.RequestBody)
			}
			if layout == "intent" {
				var snapshot struct {
					Adapter string `json:"adapter_version"`
				}
				if err := json.Unmarshal(u.RequestSnapshot, &snapshot); err != nil {
					t.Fatal(err)
				}
				if _, pre, err := f.r.RequestCancellation(f.ctx, u.ID, u.UserID, digestString([]byte("owner-"+u.ID.String())), cancellationFingerprint(u.ID), snapshot.Adapter); err != nil || pre {
					t.Fatal("real USER stop", pre, err)
				}
			}
			if name == "acquired-intent-control" || name == "model-first-control" {
				if _, _, err := f.r.requestEmergencyCancellation(f.ctx, claim, ProviderCancelUnsupported, f.service.upstream.AdapterVersion()); err != nil {
					t.Fatal("existing MODEL fallback must remain available", err)
				}
				return
			}
			var before string
			if err := f.r.pool.QueryRow(f.ctx, `SELECT to_jsonb(request)::text FROM usage_requests request WHERE id=$1`, u.ID).Scan(&before); err != nil {
				t.Fatal(err)
			}
			fundsBefore := f05CancellationClosureFundsFacts(t, f.bp, f.ctx)
			if _, _, err := f.r.requestEmergencyCancellation(f.ctx, claim, ProviderCancelUnsupported, f.service.upstream.AdapterVersion()); !errors.Is(err, ErrForbidden) {
				t.Error("locked MODEL command replaced USER proof owner", err)
			}
			if name == "after-claim" {
				if err := f.r.retryEmergencyCancellation(f.ctx, claim, ErrForbidden); err != nil {
					t.Fatal(err)
				}
			}
			if name == "waiting-owner-does-not-starve-later-target" {
				_, later := f.preparedExpiry(t, "intent")
				claimed, err := f.r.claimEmergencyCancellations(f.ctx, 1)
				if err != nil || len(claimed) != 1 || claimed[0].ID != later {
					t.Fatal("protected USER owner starved later MODEL target", claimed, err)
				}
			} else if claimed, err := f.r.claimEmergencyCancellations(f.ctx, 1); err != nil || len(claimed) != 0 {
				t.Error("queue claimed protected USER owner", claimed, err)
			}
			var after string
			var observations int
			if err := f.r.pool.QueryRow(f.ctx, `SELECT to_jsonb(request)::text,(SELECT count(*) FROM model_emergency_cancellation_evidence WHERE emergency_cancellation_id=$2) FROM usage_requests request WHERE id=$1`, u.ID, target).Scan(&after, &observations); err != nil || after != before || observations != 0 {
				t.Error("MODEL changed original USER request or evidence", observations, err)
			}
			if name != "waiting-owner-does-not-starve-later-target" && f05CancellationClosureFundsFacts(t, f.bp, f.ctx) != fundsBefore {
				t.Error("MODEL deferral changed funds")
			}
			proof := &emergencyLostExpiryProofClient{service: f.service}
			expiry := billing.NewExpiredHoldRecoveryWorker(f.funds, proof, time.Minute)
			if err := expiry.RunOnce(f.ctx); err != nil {
				t.Fatal(err)
			}
			confirmed, err := f.r.GetUsage(f.ctx, u.ID)
			if err != nil || confirmed.ExecutionStatus != "CANCELLED_BEFORE_DISPATCH" || confirmed.CancelStatus != "LOCAL_CONFIRMED" || confirmed.SettlementStatus != "RELEASED" {
				t.Fatal("original expiry could not confirm release", confirmed.ExecutionStatus, confirmed.CancelStatus, confirmed.SettlementStatus, err)
			}
			if _, err := f.bp.Exec(f.ctx, `UPDATE usage_hold_expiry_recovery SET next_attempt_at=clock_timestamp() WHERE hold_id=$1`, *u.HoldID); err != nil {
				t.Fatal(err)
			}
			if err := expiry.RunOnce(f.ctx); err != nil {
				t.Fatal(err)
			}
			var state string
			if err := f.bp.QueryRow(f.ctx, `SELECT state FROM usage_hold_expiry_recovery WHERE hold_id=$1`, *u.HoldID).Scan(&state); err != nil || state != "COMPLETED" {
				t.Fatal("lost expiry response did not recover", state, err)
			}
			worker := NewEmergencyModelCancellationWorker(f.r, f.service, time.Second)
			for range 2 {
				if err := worker.runOnce(f.ctx); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.r.pool.QueryRow(f.ctx, `SELECT status FROM model_emergency_cancellations WHERE id=$1`, target).Scan(&state); err != nil || state != "CONVERGED" {
				t.Fatal("deferred MODEL target did not converge", state, err)
			}
			assessment, err := f.r.AccountClosureAssessment(f.ctx, u.UserID)
			if err != nil || !assessment.Ready {
				t.Fatal("confirmed release still blocks closure", assessment, err)
			}
			wallet, err := f.funds.GetWallet(f.ctx, u.WalletID)
			if err != nil || wallet.ReservedMicrocredits != 0 || wallet.PostedMicrocredits != 1000 || wallet.AvailableMicrocredits != 1000 {
				t.Fatal("original wallet release", wallet, err)
			}
		})
	}
}
