package token

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type modelExpiryObservationFixture struct {
	r       *PostgresRepository
	funds   *billing.PostgresRepository
	bp      *pgxpool.Pool
	ctx     context.Context
	client  *HTTPBillingClient
	service *Service
}

func newModelExpiryObservationFixture(t *testing.T) modelExpiryObservationFixture {
	t.Helper()
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	r, ctx := retainedManagementTokenRepository(t)
	funds, _, bp := retainedManagementBillingRepository(t)
	server := httptest.NewServer(billing.NewService(funds).Handler("controlled-model-expiry"))
	t.Cleanup(server.Close)
	client, err := NewHTTPBillingClient(server.URL, "controlled-model-expiry")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewNewAPIGatewayAdapter()
	service := &Service{repository: r, billing: client, cancellationBilling: client, cancellationStore: r, cancellationAdapter: adapter, upstream: adapter}
	return modelExpiryObservationFixture{r, funds, bp, ctx, client, service}
}

func (f modelExpiryObservationFixture) prepare(t *testing.T, layout string) (UsageRequest, uuid.UUID, *billing.ExpiredHoldRecoveryWorker) {
	t.Helper()
	u, envelope, hold := jR6UnboundSeed(t, f.r, f.funds, f.bp, f.client, f.ctx)
	if layout == "hold" {
		if err := f.r.persistAcknowledgedHold(f.ctx, u.ID, hold.ID, envelope.Price.ID); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := f.r.CompleteHoldAndDispatch(f.ctx, u.ID, hold.ID, envelope.Price.ID); err != nil {
			t.Fatal(err)
		}
	}
	if layout == "user-intent" {
		if result, pre, err := f.r.RequestCancellation(f.ctx, u.ID, u.UserID, digestString([]byte("model-expiry-user-"+u.ID.String())), cancellationFingerprint(u.ID), envelope.AdapterVersion); err != nil || pre || result.CancelStatus != "UNSUPPORTED" {
			t.Fatal("original local delivery stop", result, pre, err)
		}
	}
	// Only policy metadata is controlled. Production sync derives the target;
	// BeginUsage/PrepareDispatch/reservation/ACK/intent and both workers are real.
	if _, err := f.r.pool.Exec(f.ctx, `INSERT INTO model_emergency_cancellation_policies(id,catalog_version,model_id,effective_at,requested_by,reason,created_at) VALUES($1,$2,$3,$4,$5,'controlled expiry observation',now())`, uuid.New(), *u.CatalogVersion, *u.ModelID, time.Now().UTC(), u.UserID); err != nil {
		t.Fatal(err)
	}
	if err := f.r.syncEmergencyCancellationTargets(f.ctx); err != nil {
		t.Fatal(err)
	}
	var target uuid.UUID
	if err := f.r.pool.QueryRow(f.ctx, `SELECT id FROM model_emergency_cancellations WHERE usage_request_id=$1`, u.ID).Scan(&target); err != nil {
		t.Fatal(err)
	}
	if _, err := f.bp.Exec(f.ctx, `UPDATE usage_holds SET expires_at=clock_timestamp()-interval '1 minute' WHERE id=$1`, hold.ID); err != nil {
		t.Fatal(err)
	}
	proof := &emergencyLostExpiryProofClient{service: f.service}
	expiry := billing.NewExpiredHoldRecoveryWorker(f.funds, proof, time.Minute)
	if err := expiry.RunOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	confirmed, err := f.r.GetUsage(f.ctx, u.ID)
	if err != nil || !proof.lost || confirmed.ExecutionStatus != "CANCELLED_BEFORE_DISPATCH" || confirmed.CancelStatus != "LOCAL_CONFIRMED" || confirmed.SettlementStatus != "RELEASED" || confirmed.HoldID == nil || *confirmed.HoldID != hold.ID {
		t.Fatal("original expiry did not actually confirm its exact released hold", confirmed, err)
	}
	return confirmed, target, expiry
}

func TestModelTerminalObservationPreservesExpiryProofPostgres(t *testing.T) {
	f := newModelExpiryObservationFixture(t)
	for _, layout := range []string{"hold", "intent", "user-intent"} {
		for _, stage := range []string{"requested", "attempted", "converged"} {
			t.Run(layout+"-"+stage, func(t *testing.T) {
				u, id, expiry := f.prepare(t, layout)
				before := f05CancellationClosureFundsFacts(t, f.bp, f.ctx)
				targets, err := f.r.claimEmergencyCancellations(f.ctx, 1)
				if err != nil || len(targets) != 1 || targets[0].ID != id {
					t.Fatal("real original target claim", targets, err)
				}
				pre, terminal, err := f.r.requestEmergencyCancellation(f.ctx, id, ProviderCancelUnsupported, f.service.upstream.AdapterVersion())
				if err != nil || pre || !terminal {
					t.Fatal("model did not observe the existing terminal", pre, terminal, err)
				}
				if stage != "requested" {
					if err := f.r.recordEmergencyCancellationAttempt(f.ctx, id, ProviderCancelUnsupported, false, pre, terminal); err != nil {
						t.Fatal(err)
					}
				}
				if stage == "converged" {
					if err := f.r.finalizeEmergencyCancellations(f.ctx, 100); err != nil {
						t.Fatal(err)
					}
				}
				observed, err := f.r.GetUsage(f.ctx, u.ID)
				if err != nil || observed.CancelStatus != u.CancelStatus || observed.CancellationOrigin != u.CancellationOrigin || observed.CancelRequestedAt == nil || !observed.CancelRequestedAt.Equal(*u.CancelRequestedAt) || observed.DeliveryStatus != u.DeliveryStatus {
					t.Fatal("model observation replaced original confirmation authority", observed, err)
				}
				if after := f05CancellationClosureFundsFacts(t, f.bp, f.ctx); after != before {
					t.Fatal("model observation changed financial facts")
				}
				if _, err := f.bp.Exec(f.ctx, `UPDATE usage_hold_expiry_recovery SET next_attempt_at=clock_timestamp() WHERE hold_id=$1`, *u.HoldID); err != nil {
					t.Fatal(err)
				}
				if err := expiry.RunOnce(f.ctx); err != nil {
					t.Fatal(err)
				}
				var state string
				if err := f.bp.QueryRow(f.ctx, `SELECT state FROM usage_hold_expiry_recovery WHERE hold_id=$1`, *u.HoldID).Scan(&state); err != nil || state != "COMPLETED" {
					t.Fatal("original released expiry proof did not complete after model observation", state, err)
				}
				if stage == "requested" {
					// A real worker may resume after this committed request and an
					// expired lease; do not fabricate its ATTEMPTED/CONVERGED rows.
					if _, err := f.r.pool.Exec(f.ctx, `UPDATE model_emergency_cancellations SET lease_until=$2 WHERE id=$1`, id, f.r.now().Add(-time.Second)); err != nil {
						t.Fatal(err)
					}
				}
				worker := NewEmergencyModelCancellationWorker(f.r, f.service, time.Second)
				for range 2 {
					if err := worker.runOnce(f.ctx); err != nil {
						t.Fatal(err)
					}
				}
				if err := f.r.pool.QueryRow(f.ctx, `SELECT status FROM model_emergency_cancellations WHERE id=$1`, id).Scan(&state); err != nil || state != "CONVERGED" {
					t.Fatal("original model target did not converge", state, err)
				}
				if after := f05CancellationClosureFundsFacts(t, f.bp, f.ctx); after != before {
					t.Fatal("resumed model target repeated financial effects")
				}
				wallet, err := f.funds.GetWallet(f.ctx, u.WalletID)
				if err != nil || wallet.ReservedMicrocredits != 0 || wallet.PostedMicrocredits != 1000 || wallet.AvailableMicrocredits != 1000 {
					t.Fatal("released wallet changed", wallet, err)
				}
			})
		}
	}
}

func TestModelTerminalObservationRejectsUntrustedEvidencePostgres(t *testing.T) {
	f := newModelExpiryObservationFixture(t)
	u, target, _ := f.prepare(t, "intent")
	targets, err := f.r.claimEmergencyCancellations(f.ctx, 1)
	if err != nil || len(targets) != 1 || targets[0].ID != target {
		t.Fatal("original model claim", targets, err)
	}
	pre, terminal, err := f.r.requestEmergencyCancellation(f.ctx, target, ProviderCancelUnsupported, f.service.upstream.AdapterVersion())
	if err != nil || pre || !terminal {
		t.Fatal("original model terminal observation", pre, terminal, err)
	}
	if err := f.r.recordEmergencyCancellationAttempt(f.ctx, target, ProviderCancelUnsupported, false, pre, terminal); err != nil {
		t.Fatal(err)
	}
	// Pause before CONVERGED so each adversarial convergence insert is new,
	// rather than being discarded by the production audit deduplication key.

	checked, err := f.r.loadUnusedExpiryDispatch(f.ctx, u.ID)
	if err != nil || checked == nil {
		t.Fatal("real sealed unused intent", err)
	}
	defer checked.clear()
	before := f05CancellationClosureFundsFacts(t, f.bp, f.ctx)
	for index, name := range []string{"orphan-request", "pre-dispatch", "not-terminal", "provider-success", "unknown-capability", "extra-field", "missing-field", "wrong-adapter", "provider-reference", "upstream-reference", "attempt-releases", "attempt-aborts", "attempt-provider-success", "converged-extra-field", "convergence-wrong-source"} {
		t.Run(name, func(t *testing.T) {
			// Adversarial append-only audit inputs in a rollback-only transaction;
			// these are negative classification probes, not claimed public writes.
			tx, err := f.r.pool.Begin(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(f.ctx)
			kind := "REQUESTED"
			if strings.HasPrefix(name, "attempt-") {
				kind = "ATTEMPTED"
			}
			if strings.HasPrefix(name, "converg") {
				kind = "CONVERGED"
			}
			var payload map[string]any
			if kind == "CONVERGED" {
				payload = map[string]any{"usage_request_id": u.ID, "execution_status": "CANCELLED_BEFORE_DISPATCH", "cancel_status": "LOCAL_CONFIRMED", "settlement_status": "RELEASED", "hold_id": u.HoldID, "actual_microcredits": nil, "usage_snapshot": []byte(u.UsageSnapshot), "emergency_removal_implied_free": false}
			} else {
				var raw []byte
				if err := tx.QueryRow(f.ctx, `SELECT snapshot FROM model_emergency_cancellation_evidence WHERE emergency_cancellation_id=$1 AND evidence_type=$2 ORDER BY created_at,id LIMIT 1`, target, kind).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(raw, &payload); err != nil {
					t.Fatal(err)
				}
			}
			switch name {
			case "pre-dispatch":
				payload["pre_dispatch"] = true
			case "not-terminal":
				payload["already_terminal"] = false
			case "provider-success", "attempt-provider-success":
				payload["provider_success_inferred"] = true
			case "unknown-capability":
				payload["capability"] = "UNKNOWN"
			case "extra-field", "converged-extra-field":
				payload["unexpected"] = true
			case "missing-field":
				delete(payload, "provider_success_inferred")
			case "attempt-releases":
				payload["hold_released"] = true
			case "attempt-aborts":
				payload["local_transport_aborted"] = true
			}
			at := f.r.now().Add(time.Duration(index+1) * time.Second)
			if name != "orphan-request" {
				if _, err := tx.Exec(f.ctx, `INSERT INTO model_emergency_cancellation_evidence(id,emergency_cancellation_id,evidence_type,snapshot,created_at) VALUES($1,$2,$3,$4,$5)`, uuid.New(), target, kind, payload, at); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "CONVERGED" {
				source := "RECONCILER"
				if name == "convergence-wrong-source" {
					source = "GATEWAY"
				}
				if err := appendUsageConvergenceEvidenceTx(f.ctx, tx, u.ID, "CANCELLATION", source, "RECORDED", "model-emergency-converged", payload, at); err != nil {
					t.Fatal(err)
				}
				var inserted int
				if err := tx.QueryRow(f.ctx, `SELECT count(*) FROM usage_convergence_evidence WHERE usage_request_id=$1 AND observed_at=$2 AND source_type=$3`, u.ID, at, source).Scan(&inserted); err != nil || inserted != 1 {
					t.Fatal("negative audit input was not actually persisted", inserted, err)
				}
			} else {
				adapter := f.service.upstream.AdapterVersion()
				if kind == "ATTEMPTED" {
					adapter = "gateway"
				}
				if name == "wrong-adapter" {
					adapter = " "
				}
				var provider, reference any
				if name == "provider-reference" {
					provider = "controlled"
				}
				if name == "upstream-reference" {
					reference = strings.Repeat("a", 64)
				}
				if _, err := tx.Exec(f.ctx, `INSERT INTO usage_cancellation_evidence(id,usage_request_id,evidence_type,adapter_version,provider_id,upstream_reference_digest,sanitized_evidence,occurred_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, uuid.New(), u.ID, "MODEL_EMERGENCY_"+kind, adapter, provider, reference, payload, at); err != nil {
					t.Fatal(err)
				}
			}
			if err := validateUnusedExpiryDispatchTx(f.ctx, tx, u, checked); !errors.Is(err, ErrForbidden) {
				t.Fatal("untrusted observation bypassed original unused-dispatch proof", err)
			}
		})
	}
	if after := f05CancellationClosureFundsFacts(t, f.bp, f.ctx); after != before {
		t.Fatal("negative audit probes changed financial facts")
	}
}
