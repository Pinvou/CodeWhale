package token

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/google/uuid"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestManagementEmergencyCancellationRetriesConfirmedHoldRelease(t *testing.T) {
	testEmergencyConfirmedHoldRecovery(t, "confirmed")
}
func TestManagementEmergencyCancellationRetriesRecoveredACKRelease(t *testing.T) {
	testEmergencyConfirmedHoldRecovery(t, "recovered-ack")
}
func TestManagementEmergencyCancellationPreservesPriorUserRequest(t *testing.T) {
	for _, mode := range []string{"prior-user", "prior-user-review", "prior-user-review-full-batch", "prior-user-review-proof-retry"} {
		t.Run(mode, func(t *testing.T) { testEmergencyConfirmedHoldRecovery(t, mode) })
	}
}
func TestManagementEmergencyCancellationRetriesCanonicalZeroBudget(t *testing.T) {
	testEmergencyConfirmedHoldRecovery(t, "zero-budget")
}

type emergencyLostExpiryProofClient struct {
	service *Service
	lost    bool
}

func (c *emergencyLostExpiryProofClient) FenceExpiredHold(ctx context.Context, identity billing.ExpiredHoldIdentity) (billing.ExpiredHoldProof, error) {
	proof, err := c.service.recoverExpiredHold(ctx, identity)
	if err == nil && !c.lost {
		c.lost = true
		return billing.ExpiredHoldProof{}, errors.New("controlled lost confirmed expiry proof response")
	}
	return proof, err
}

func testEmergencyConfirmedHoldRecovery(t *testing.T, mode string) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	repository, ctx := retainedManagementTokenRepository(t)
	funds, _, billingPool := retainedManagementBillingRepository(t)
	user := uuid.New()
	wallet, _, err := funds.EnsureWallet(ctx, "PERSONAL", user)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = funds.Credit(ctx, wallet.ID, "worker-credit-"+user.String(), 1000, false); err != nil {
		t.Fatal(err)
	}
	usageID := retainedCancellationUsage(t, repository, ctx, user, wallet.ID, nil, "HOLD_RESERVING")
	envelope, err := repository.LoadDispatchPreparation(ctx, usageID)
	if err != nil {
		t.Fatal(err)
	}
	amount := int64(100)
	if mode == "zero-budget" {
		// The helper above installs only account/credential resources for this case.
		// Produce a separate canonical usage with real Cost/BeginUsage/PrepareDispatch;
		// never UPDATE its maximum, immutable body, hold or execution state to zero.
		envelope.Price.ProtocolCode = "CHAT"
		envelope.Price.CreditUnit = "MICROCREDIT"
		envelope.Price.FormulaSnapshot = json.RawMessage(`{"evaluator_version":"SAFE_PARAMETER_MAP_V1","expression":{"INPUT_TOKEN":"estimated_input_tokens","CACHED_INPUT_TOKEN":"cached_input_tokens","OUTPUT_TOKEN":"max_output_tokens"}}`)
		for _, unit := range []string{"INPUT_TOKEN", "CACHED_INPUT_TOKEN", "OUTPUT_TOKEN"} {
			envelope.Price.Units = append(envelope.Price.Units, ResolvedUnitPrice{UnitCode: unit, MinimumPrecision: 1, PriceDenominator: 1000000, RoundingRule: "CEILING", ResolvedPrice: 0})
		}
		if err = ValidatePriceFormula(envelope.Price); err != nil {
			t.Fatal("valid controlled free price", err)
		}
		amount, err = Cost(envelope.Price, Usage{InputTokens: 12, OutputTokens: 25, CachedInputTokens: 3})
		if err != nil || amount != 0 {
			t.Fatal("actual free tariff cost", amount, err)
		}
		seed, e := repository.GetUsage(ctx, usageID)
		if e != nil {
			t.Fatal(e)
		}
		usageID = uuid.New()
		request := UsageRequest{ID: usageID, IdempotencyNamespace: "model-inference", IdempotencyKeyDigest: replayDigest([]byte(usageID.String())), CanonicalVersion: 1, RequestFingerprint: replayDigest([]byte("zero-request:" + usageID.String())), UserID: user, SessionID: "owned-zero-session", TokenAccountID: seed.TokenAccountID, CredentialID: seed.CredentialID, WalletID: wallet.ID, Model: "model", MaximumMicrocredits: amount}
		negative := request
		negative.MaximumMicrocredits = -1
		if _, _, e := repository.BeginUsage(ctx, negative, map[string]any{}); !errors.Is(e, ErrForbidden) {
			t.Fatal("negative budget admitted", e)
		}
		if _, created, e := repository.BeginUsage(ctx, request, map[string]any{"context_type": "PERSONAL", "protocol_code": "CHAT", "price_version_id": envelope.Price.ID, "pricing_snapshot": envelope.Price}); e != nil || !created {
			t.Fatal("real zero canonical admission", e)
		}
		envelope.BaseURL = "https://provider.invalid"
		envelope.Credential = []byte("controlled-provider-fact")
		envelope.RequestBody = []byte(`{"model":"model"}`)
		envelope.MaximumCost = amount
		if e := repository.PrepareDispatch(ctx, usageID, envelope); e != nil {
			t.Fatal("real zero dispatch preparation", e)
		}
		if e := repository.MarkHoldReserving(ctx, usageID); e != nil {
			t.Fatal("real zero reserve transition", e)
		}
	}
	// An existing persisted price and immutable request are used by both the lost
	// first response and the later recovery request; no alternate facts are passed.
	if _, err = billingPool.Exec(ctx, `INSERT INTO pricing_versions(id,model,input_per_million,output_per_million,cached_input_per_million,effective_from,created_at,scope_id,published_at) VALUES($1,'model',0,0,0,now(),now(),'model',now())`, envelope.Price.ID); err != nil {
		t.Fatal(err)
	}
	usage, err := repository.GetUsage(ctx, usageID)
	if err != nil {
		t.Fatal(err)
	}
	priceJSON, err := json.Marshal(envelope.Price)
	if err != nil {
		t.Fatal(err)
	}
	pricing := &billing.HoldPricingContext{ModelID: usage.Model, ProtocolCode: "CHAT", FormulaVersionID: envelope.Price.FormulaVersionID, PricingSnapshot: priceJSON, RequestSnapshot: usage.RequestSnapshot}
	actualHold, err := funds.ReservePriced(ctx, usageID, wallet.ID, amount, &envelope.Price.ID, time.Now().Add(time.Hour), pricing)
	if err != nil {
		t.Fatal(err)
	}
	// Unlike the lost-reservation-ACK case, Token already durably acknowledged
	// this precise hold before the emergency request and failed release.
	// The shared historical fixture starts with a user-cancel timestamp. This
	// scenario starts before any cancellation, as the production producer does.
	if _, err = repository.pool.Exec(ctx, `UPDATE usage_requests SET cancel_status='NOT_REQUESTED',cancel_requested_at=NULL WHERE id=$1`, usageID); err != nil {
		t.Fatal(err)
	}
	if mode != "recovered-ack" {
		if err = repository.persistAcknowledgedHold(ctx, usageID, actualHold.ID, envelope.Price.ID); err != nil {
			t.Fatal(err)
		}
	}
	var originalUserTime *time.Time
	if mode == "prior-user" {
		if _, pre, e := repository.RequestCancellation(ctx, usageID, user, strings.Repeat("d", 64), cancellationFingerprint(usageID), "owned-user-adapter"); e != nil || !pre {
			t.Fatal("real prior user cancellation", e)
		}
		original, e := repository.GetUsage(ctx, usageID)
		if e != nil || original.CancelRequestedAt == nil {
			t.Fatal("original user time", e)
		}
		originalUserTime = original.CancelRequestedAt
	}
	modelID, policyID := uuid.New(), uuid.New()
	var catalogVersion int64
	if _, err = repository.pool.Exec(ctx, `INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at) VALUES($1,'controlled','new-api-v1','model',$2,$3,now())`, modelID, strings.Repeat("c", 64), user); err != nil {
		t.Fatal(err)
	}
	if err = repository.pool.QueryRow(ctx, `INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'controlled lifecycle fact',$2,now(),now()) RETURNING version`, uuid.New(), user).Scan(&catalogVersion); err != nil {
		t.Fatal(err)
	}
	capabilityID := uuid.New()
	protocolID := uuid.MustParse("00000000-0000-7000-8200-000000000000")
	if _, err = repository.pool.Exec(ctx, `INSERT INTO model_capability_versions(id,model_id,version,protocol_definition_id,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,created_by,created_at) VALUES($1,$2,1,$3,1000,100,'["text"]','controlled',$4,now())`, capabilityID, modelID, protocolID, user); err != nil {
		t.Fatal(err)
	}
	if _, err = repository.pool.Exec(ctx, `UPDATE usage_requests SET model_id=$2,catalog_version=$3,model_capability_version_id=$4,protocol_definition_id=$5,route_generation=1,model_snapshot='{}',started_at=now()-interval '1 hour' WHERE id=$1`, usageID, modelID, catalogVersion, capabilityID, protocolID); err != nil {
		t.Fatal(err)
	}

	if _, err = repository.pool.Exec(ctx, `INSERT INTO model_emergency_cancellation_policies(id,catalog_version,model_id,effective_at,requested_by,reason,created_at) VALUES($1,$2,$3,$5,$4,'controlled force cancellation',now())`, policyID, catalogVersion, modelID, user, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var allowed atomic.Bool
	var reserveAllowed atomic.Bool
	reserveAllowed.Store(mode != "recovered-ack")
	handler := billing.NewService(funds).Handler("controlled-service")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (!allowed.Load() && r.URL.Path == "/internal/v1/holds/release") || (!reserveAllowed.Load() && r.URL.Path == "/internal/v1/holds/reserve") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(403)
			io.WriteString(w, `{"code":"FORBIDDEN"}`)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	client, err := NewHTTPBillingClient(server.URL, "controlled-service")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewNewAPIGatewayAdapter()
	service := &Service{billing: client, cancellationBilling: client, cancellationStore: repository, cancellationAdapter: adapter, upstream: adapter}
	worker := NewEmergencyModelCancellationWorker(repository, service, time.Second)
	if strings.HasPrefix(mode, "prior-user-review") {
		// Queue the MODEL target while the real hold is still reserved, then
		// let the original USER request encounter an unconfirmed release.
		if err = repository.syncEmergencyCancellationTargets(ctx); err != nil {
			t.Fatal(err)
		}
		if _, pre, e := repository.RequestCancellation(ctx, usageID, user, strings.Repeat("e", 64), cancellationFingerprint(usageID), adapter.AdapterVersion()); e != nil || !pre {
			t.Fatal("original user cancellation", e)
		}
		if _, e := service.finishPreDispatchCancellation(ctx, usageID); e == nil {
			t.Fatal("controlled release rejection did not retain uncertainty")
		}
		before := f05CancellationRecoveryFacts(t, repository, ctx, usageID)
		if err = worker.runOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if after := f05CancellationRecoveryFacts(t, repository, ctx, usageID); after != before {
			t.Fatal("model target overwrote the original USER recovery state or evidence")
		}
		var origin, target string
		if err = repository.pool.QueryRow(ctx, `SELECT request.cancellation_origin,cancellation.status FROM usage_requests request JOIN model_emergency_cancellations cancellation ON cancellation.usage_request_id=request.id WHERE request.id=$1`, usageID).Scan(&origin, &target); err != nil || origin != "USER" || target != "PENDING" {
			t.Fatal("original recovery ownership was not retained", origin, target, err)
		}
		money, e := funds.GetWallet(ctx, wallet.ID)
		if e != nil || money.ReservedMicrocredits != amount {
			t.Fatal("deferred model target changed reserved funds", money, e)
		}
		if mode != "prior-user-review-proof-retry" {
			// A full batch of waiting USER recoveries must not starve later
			// runnable MODEL targets. A batch of one fixes that boundary without
			// timing assumptions; both targets use the production sync/worker.
			if mode == "prior-user-review" {
				worker.batchSize = 1
			}
			// Also exercise the production default limit: waiting rows must be
			// excluded before LIMIT, leaving the whole runnable batch available.
			laterIDs := make([]uuid.UUID, 0, worker.batchSize)
			for range worker.batchSize {
				laterUser := uuid.New()
				laterWallet, _, e := funds.EnsureWallet(ctx, "PERSONAL", laterUser)
				if e != nil {
					t.Fatal(e)
				}
				later := retainedCancellationUsage(t, repository, ctx, laterUser, laterWallet.ID, nil, "STREAMING")
				if _, err = repository.pool.Exec(ctx, `UPDATE usage_requests SET model_id=$2,started_at=$3,catalog_version=$4,model_capability_version_id=$5,protocol_definition_id=$6,route_generation=1,model_snapshot='{}',cancel_status='NOT_REQUESTED',cancel_requested_at=NULL WHERE id=$1`, later, modelID, usage.StartedAt, catalogVersion, capabilityID, protocolID); err != nil {
					t.Fatal(err)
				}
				laterIDs = append(laterIDs, later)
			}
			clock := repository.now
			laterTime := clock().Add(time.Second)
			repository.now = func() time.Time { return laterTime }
			err = repository.syncEmergencyCancellationTargets(ctx)
			repository.now = clock
			if err != nil {
				t.Fatal(err)
			}
			if err = worker.runOnce(ctx); err != nil {
				t.Fatal(err)
			}
			var requests int
			if err = repository.pool.QueryRow(ctx, `SELECT count(*) FROM model_emergency_cancellation_evidence evidence JOIN model_emergency_cancellations cancellation ON cancellation.id=evidence.emergency_cancellation_id WHERE cancellation.usage_request_id=ANY($1) AND evidence.evidence_type='REQUESTED'`, laterIDs).Scan(&requests); err != nil || requests != len(laterIDs) {
				t.Fatal("waiting USER recovery starved a later MODEL target", requests, err)
			}
			if after := f05CancellationRecoveryFacts(t, repository, ctx, usageID); after != before {
				t.Fatal("processing the later target changed original USER recovery facts")
			}
		}
		// Exercise the existing proof owner and real Billing release. Only this
		// fresh fixture's hold is made due; no production recovery gate changes.
		allowed.Store(true)
		service.repository = repository
		if _, err = billingPool.Exec(ctx, `UPDATE usage_holds SET expires_at=clock_timestamp()-interval '1 minute' WHERE id=$1`, actualHold.ID); err != nil {
			t.Fatal(err)
		}
		var proofClient billing.ExpiredHoldProofClient = f04LocalExpiryClient{service}
		if mode == "prior-user-review-proof-retry" {
			proofClient = &emergencyLostExpiryProofClient{service: service}
		}
		expiry := billing.NewExpiredHoldRecoveryWorker(funds, proofClient, time.Minute)
		if err = expiry.RunOnce(ctx); err != nil {
			t.Fatal("original USER proof could not recover release", err)
		}
		final, e := repository.GetUsage(ctx, usageID)
		if e != nil || final.ExecutionStatus != "CANCELLED_BEFORE_DISPATCH" || final.CancelStatus != "LOCAL_CONFIRMED" || final.SettlementStatus != "RELEASED" {
			t.Fatal("original recovery did not confirm its exact hold", final, e)
		}
		beforeFunds := f05CancellationClosureFundsFacts(t, billingPool, ctx)
		if err = worker.runOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if err = worker.runOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if after := f05CancellationClosureFundsFacts(t, billingPool, ctx); after != beforeFunds {
			t.Fatal("terminal MODEL observation repeated financial effects")
		}
		if mode == "prior-user-review-proof-retry" {
			var state string
			if err = billingPool.QueryRow(ctx, `SELECT state FROM usage_hold_expiry_recovery WHERE hold_id=$1`, actualHold.ID).Scan(&state); err != nil || state != "MANUAL_REVIEW" {
				t.Fatal("lost proof response did not retain a retryable recovery", state, err)
			}
			if _, err = billingPool.Exec(ctx, `UPDATE usage_hold_expiry_recovery SET next_attempt_at=clock_timestamp() WHERE hold_id=$1`, actualHold.ID); err != nil {
				t.Fatal(err)
			}
			if err = expiry.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if err = billingPool.QueryRow(ctx, `SELECT state FROM usage_hold_expiry_recovery WHERE hold_id=$1`, actualHold.ID).Scan(&state); err != nil || state != "COMPLETED" {
				t.Fatal("MODEL terminal observation broke original proof replay", state, err)
			}
			if after := f05CancellationClosureFundsFacts(t, billingPool, ctx); after != beforeFunds {
				t.Fatal("proof replay repeated financial effects")
			}
		}
		if err = repository.pool.QueryRow(ctx, `SELECT status FROM model_emergency_cancellations WHERE usage_request_id=$1`, usageID).Scan(&target); err != nil || target != "CONVERGED" {
			t.Fatal("MODEL target did not converge after original recovery", target, err)
		}
		money, e = funds.GetWallet(ctx, wallet.ID)
		if e != nil || money.ReservedMicrocredits != 0 || money.AvailableMicrocredits != 1000 {
			t.Fatal("original recovery did not restore exact wallet funds", money, e)
		}
		return
	}
	if err = worker.runOnce(ctx); err != nil {
		t.Fatal(err)
	}
	first, err := repository.GetUsage(ctx, usageID)
	if err != nil {
		t.Fatal(err)
	}
	wallet, err = funds.GetWallet(ctx, wallet.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.ExecutionStatus != "REVIEW" || first.CancelStatus == "LOCAL_CONFIRMED" || wallet.ReservedMicrocredits != amount {
		t.Fatalf("first rejection lost uncertainty: %s/%s reserved=%d", first.ExecutionStatus, first.CancelStatus, wallet.ReservedMicrocredits)
	}
	var cancellationID uuid.UUID
	if err = repository.pool.QueryRow(ctx, `SELECT id FROM model_emergency_cancellations WHERE usage_request_id=$1`, usageID).Scan(&cancellationID); err != nil {
		t.Fatal(err)
	}
	// A request and a completed reservation do not themselves prove release.
	if err = repository.recordEmergencyCancellationAttempt(ctx, cancellationID, ProviderCancelUnsupported, false, true, false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("unconfirmed attempt accepted: %v", err)
	}
	var attempted int
	if err = repository.pool.QueryRow(ctx, `SELECT count(*) FROM model_emergency_cancellation_evidence WHERE emergency_cancellation_id=$1 AND evidence_type='ATTEMPTED'`, cancellationID).Scan(&attempted); err != nil || attempted != 0 {
		t.Fatalf("unconfirmed attempt evidence=%d err=%v", attempted, err)
	}
	// Lost ACK: real reservation recovery now succeeds but release still fails.
	reserveAllowed.Store(true)
	// A second failed tick must preserve money and REVIEW rather than invent
	// confirmation. The final positive tick below uses the real Billing client.
	if err = worker.runOnce(ctx); err != nil {
		t.Fatal(err)
	}
	still, err := repository.GetUsage(ctx, usageID)
	if err != nil || still.ExecutionStatus != "REVIEW" || still.CancelStatus != "CANCEL_REQUESTED" {
		t.Fatal("continued failure lost review", err)
	}
	if still.HoldID == nil || *still.HoldID != actualHold.ID {
		t.Fatal("original hold ACK not recovered")
	}
	stillMoney, err := funds.GetWallet(ctx, wallet.ID)
	if err != nil || stillMoney.ReservedMicrocredits != amount {
		t.Fatal("continued failure changed reservation", err)
	}
	for _, negative := range []string{"wrong-error", "wrong-request", "persisted-intent", "negative-budget"} {
		t.Run(negative, func(t *testing.T) {
			tx, err := repository.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			var locked UsageRequest
			if err = scanUsage(tx.QueryRow(ctx, `SELECT `+usageSelectColumns+` FROM usage_requests WHERE id=$1 FOR UPDATE`, usageID), &locked); err != nil {
				t.Fatal(err)
			}
			id := cancellationID
			if negative == "negative-budget" {
				locked.MaximumMicrocredits = -1
			}
			if negative == "wrong-error" {
				wrong := "ordinary-upstream-uncertainty"
				locked.ErrorClass = &wrong
			}
			if negative == "wrong-request" {
				id = uuid.New()
			}
			if negative == "persisted-intent" {
				// Adversarial historical state in a rollback-only transaction. New
				// writes are normally prevented by guard_usage_dispatch_cancellation;
				// do not disable that trigger or persist the injected state.
				if _, err = tx.Exec(ctx, `UPDATE usage_requests SET execution_status='HOLD_RESERVED',cancel_status='NOT_REQUESTED' WHERE id=$1`, usageID); err != nil {
					t.Fatal(err)
				}
				if _, err = tx.Exec(ctx, `INSERT INTO usage_dispatch_intents(usage_request_id,encrypted_envelope,key_id,envelope_integrity,upstream_idempotency_key,created_at) SELECT usage_request_id,encrypted_envelope,key_id,envelope_integrity,$2,created_at FROM usage_dispatch_preparations WHERE usage_request_id=$1`, usageID, "negative-owned-intent-"+usageID.String()); err != nil {
					t.Fatal(err)
				}
				if _, err = tx.Exec(ctx, `UPDATE usage_requests SET execution_status='REVIEW',cancel_status='CANCEL_REQUESTED' WHERE id=$1`, usageID); err != nil {
					t.Fatal(err)
				}
			}
			if err = repository.restoreEmergencyPreDispatchReview(ctx, tx, locked, id); !errors.Is(err, ErrForbidden) {
				t.Fatal("unproved review unfenced", err)
			}
			var state string
			if err = tx.QueryRow(ctx, `SELECT execution_status FROM usage_requests WHERE id=$1`, usageID).Scan(&state); err != nil || state != "REVIEW" {
				t.Fatal("negative changed review", state, err)
			}
		})
	}
	allowed.Store(true)
	if err = worker.runOnce(ctx); err != nil {
		t.Fatal(err)
	}
	final, err := repository.GetUsage(ctx, usageID)
	if err != nil {
		t.Fatal(err)
	}
	wallet, err = funds.GetWallet(ctx, wallet.ID)
	if err != nil {
		t.Fatal(err)
	}
	var targetStatus string
	if err = repository.pool.QueryRow(ctx, `SELECT status FROM model_emergency_cancellations WHERE usage_request_id=$1`, usageID).Scan(&targetStatus); err != nil {
		t.Fatal(err)
	}
	if final.ExecutionStatus != "CANCELLED_BEFORE_DISPATCH" || final.SettlementStatus != "RELEASED" || final.CancelStatus != "LOCAL_CONFIRMED" || final.HoldID == nil || *final.HoldID != actualHold.ID || wallet.ReservedMicrocredits != 0 || targetStatus != "CONVERGED" {
		t.Fatalf("recovery falsely confirmed or failed funds release: %s/%s/%s attached=%v reserved=%d target=%s", final.ExecutionStatus, final.SettlementStatus, final.CancelStatus, final.HoldID != nil, wallet.ReservedMicrocredits, targetStatus)
	}
	if originalUserTime != nil && (final.CancelRequestedAt == nil || !final.CancelRequestedAt.Equal(*originalUserTime) || final.UserStoppedAt == nil || !final.UserStoppedAt.Equal(*originalUserTime)) {
		t.Fatal("model takeover overwrote original user stop")
	}
	if mode == "zero-budget" {
		var allocations int
		var maximum int64
		var holdStatus string
		if e := billingPool.QueryRow(ctx, `SELECT max_microcredits,status,(SELECT count(*) FROM usage_hold_allocations WHERE hold_id=h.id) FROM usage_holds h WHERE id=$1`, actualHold.ID).Scan(&maximum, &holdStatus, &allocations); e != nil || maximum != 0 || holdStatus != "RELEASED" || allocations != 0 || wallet.PostedMicrocredits != 1000 || wallet.AvailableMicrocredits != 1000 {
			t.Fatal("zero hold financial facts", maximum, holdStatus, allocations, e)
		}
	}
	var localCount int
	if err = repository.pool.QueryRow(ctx, `SELECT count(*) FROM usage_cancellation_evidence WHERE usage_request_id=$1 AND evidence_type='LOCAL_CONFIRMED' AND sanitized_evidence->>'hold_released'='true'`, usageID).Scan(&localCount); err != nil || localCount != 1 {
		t.Fatalf("real release evidence count=%d err=%v", localCount, err)
	}
	var before, after string
	const billingFacts = `SELECT jsonb_build_object('holds',(SELECT jsonb_agg(to_jsonb(h) ORDER BY h.id) FROM usage_holds h),'leases',(SELECT jsonb_agg(to_jsonb(e) ORDER BY e.id) FROM usage_hold_lease_events e),'journal',(SELECT jsonb_agg(to_jsonb(j) ORDER BY j.id) FROM journal_entries j),'postings',(SELECT jsonb_agg(to_jsonb(p) ORDER BY p.id) FROM ledger_postings p))::text`
	if err = billingPool.QueryRow(ctx, billingFacts).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err = worker.runOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err = billingPool.QueryRow(ctx, billingFacts).Scan(&after); err != nil || before != after {
		t.Fatalf("repeat worker changed hold or journal: err=%v same=%v", err, before == after)
	}
	if err = repository.pool.QueryRow(ctx, `SELECT count(*) FROM usage_cancellation_evidence WHERE usage_request_id=$1 AND evidence_type='LOCAL_CONFIRMED'`, usageID).Scan(&localCount); err != nil || localCount != 1 {
		t.Fatalf("repeat confirmation evidence=%d err=%v", localCount, err)
	}
}
