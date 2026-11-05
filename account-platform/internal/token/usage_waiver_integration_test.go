package token

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/usagewaiver"
	"github.com/google/uuid"
)

// Only the human identity/Account authorization authority and historical input
// timestamps are controlled. Both RPC clients, handlers, public migrations and
// Token/Billing repositories run their real non-owner runtime actors.
func TestUsageWaiverCoordinatorRecoveryPostgres(t *testing.T) {
	r, ctx := retainedManagementTokenRepository(t)
	funds, _, bp := retainedManagementBillingRepository(t)
	signed := walletConsumerWorkload(t)
	t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", `[{"subject":"billing","service":"billing-service","permissions":["internal.token.usage.waive"]},{"subject":"token","service":"token-gateway","permissions":["internal.billing.hold.waive","internal.billing.hold.reserve","internal.billing.hold.extend","internal.billing.hold.commit","internal.billing.hold.release","internal.billing.wallet.ensure","internal.billing.wallet.read"]},{"subject":"wrong","service":"account-service","permissions":["internal.billing.wallet.read"]}]`)
	actor := uuid.New()
	var authorized atomic.Bool
	authorized.Store(true)
	authority := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		var input map[string]any
		if json.NewDecoder(q.Body).Decode(&input) != nil || input["issuer"] != "https://controlled-waiver.invalid" || input["subject"] != "operator" {
			t.Error("authorizer lost verified actor")
		}
		w.Header().Set("X-Authorization-Version", "19")
		json.NewEncoder(w).Encode(map[string]any{"allowed": authorized.Load(), "user_id": actor})
	}))
	defer authority.Close()
	authz, err := authorization.NewClient(authority.URL, signed("billing"))
	if err != nil {
		t.Fatal(err)
	}
	verify := func(_ context.Context, raw string) (identity.Principal, error) {
		if raw != "controlled-admin" {
			return identity.Principal{}, errors.New("controlled invalid identity")
		}
		return identity.Principal{Issuer: "https://controlled-waiver.invalid", Subject: "operator", AuthenticatedAt: time.Now()}, nil
	}
	var dropBilling, dropToken, unavailableBeforeBilling atomic.Bool
	var billingCalls, tokenCalls, ordinaryMoneyCalls, ordinaryReleaseCalls atomic.Int64
	billingService := billing.NewService(funds).WithAuthorizer(authz)
	billingHandler := billingService.Handler(signed("token"), verify)
	billingHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.URL.Path == "/internal/v1/holds/release" {
			ordinaryReleaseCalls.Add(1)
		}
		if q.URL.Path == "/internal/v1/holds/commit" || q.URL.Path == "/internal/v1/holds/extend" {
			ordinaryMoneyCalls.Add(1)
		}
		if q.URL.Path != "/internal/v1/holds/waiver" {
			billingHandler.ServeHTTP(w, q)
			return
		}
		billingCalls.Add(1)
		if unavailableBeforeBilling.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		rec := httptest.NewRecorder()
		billingHandler.ServeHTTP(rec, q)
		if dropBilling.Swap(false) {
			connection, _, e := w.(http.Hijacker).Hijack()
			if e != nil {
				t.Error(e)
			} else {
				connection.Close()
			}
			return
		}
		for key, values := range rec.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(rec.Code)
		w.Write(rec.Body.Bytes())
	}))
	defer billingHTTP.Close()
	client, err := NewHTTPBillingClient(billingHTTP.URL, signed("token"))
	if err != nil {
		t.Fatal(err)
	}
	tokenService := NewService(nil, nil, client).WithManagement(r, nil, ProviderURLPolicy{}, signed("billing"), verify)
	tokenHandler := tokenService.Handler(verify)
	tokenHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.URL.Path != "/internal/v1/usage-waivers" {
			tokenHandler.ServeHTTP(w, q)
			return
		}
		tokenCalls.Add(1)
		rec := httptest.NewRecorder()
		tokenHandler.ServeHTTP(rec, q)
		if dropToken.Swap(false) {
			connection, _, e := w.(http.Hijacker).Hijack()
			if e != nil {
				t.Error(e)
			} else {
				connection.Close()
			}
			return
		}
		for key, values := range rec.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(rec.Code)
		w.Write(rec.Body.Bytes())
	}))
	defer tokenHTTP.Close()
	coordinator, err := billing.NewHTTPUsageWaiverTokenClient(tokenHTTP.URL, signed("billing"), nil)
	if err != nil {
		t.Fatal(err)
	}
	billingService.WithUsageWaiverClient(coordinator)
	human := func(hold uuid.UUID, key, body, credential string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/admin/v1/usage-holds/"+hold.String()+"/release", strings.NewReader(body))
		if credential != "" {
			req.Header.Set("Authorization", "Bearer "+credential)
		}
		req.Header.Set("Idempotency-Key", key)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		billingHandler.ServeHTTP(rec, req)
		return rec
	}
	seed := func(t *testing.T, maximum int64, age time.Duration) (uuid.UUID, billing.Hold, uuid.UUID) {
		t.Helper()
		user := uuid.New()
		wallet, _, err := funds.EnsureWallet(ctx, "PERSONAL", user)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := funds.Credit(ctx, wallet.ID, "waiver-coordinator:"+user.String(), 100, false); err != nil {
			t.Fatal(err)
		}
		usageID := retainedCancellationUsage(t, r, ctx, user, wallet.ID, nil, "REVIEW")
		hold, err := funds.Reserve(ctx, usageID, wallet.ID, maximum, nil, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.pool.Exec(ctx, `UPDATE usage_requests SET hold_id=$2,status='REVIEW',execution_status='REVIEW',settlement_status='REVIEW',maximum_microcredits=$3,cancel_status='NOT_REQUESTED',cancel_requested_at=NULL,started_at=$4,usage_snapshot='{"provider_observation":"original","usage":{"input_tokens":2,"output_tokens":3,"cached_input_tokens":0}}',trusted_usage_snapshot='{"input_tokens":2,"output_tokens":3,"cached_input_tokens":0}',trusted_usage_source='PROVIDER_FINAL_USAGE' WHERE id=$1`, usageID, hold.ID, maximum, time.Now().UTC().Add(-age)); err != nil {
			t.Fatal(err)
		}
		if _, err := r.pool.Exec(ctx, `INSERT INTO usage_attempts(id,usage_request_id,status,input_tokens,output_tokens,cached_input_tokens,provider_id,adapter_version,upstream_request_id,upstream_reference_digest,started_at) VALUES($1,$2,'STREAMING',2,3,0,'controlled','v1',$3,encode(digest($3::text,'sha256'),'hex'),now())`, uuid.New(), usageID, "original-provider:"+usageID.String()); err != nil {
			t.Fatal(err)
		}
		return usageID, hold, wallet.ID
	}
	for _, fault := range []string{"none", "billing-ack-lost", "token-ack-lost", "unknown-crosses-deadline"} {
		t.Run(fault, func(t *testing.T) {
			usageID, hold, walletID := seed(t, 40, time.Hour)
			key := uuid.NewString()
			body := `{"reason":"explicit administrator waiver"}`
			beforeBilling, beforeToken := billingCalls.Load(), tokenCalls.Load()
			for _, bad := range []struct {
				credential, body string
				status           int
			}{{"", body, 401}, {"controlled-admin", `{"reason":"reason","actor":"forged"}`, 400}} {
				rec := human(hold.ID, key, bad.body, bad.credential)
				if rec.Code != bad.status {
					t.Fatal("human negative", rec.Code, rec.Body.String())
				}
			}
			authorized.Store(false)
			rec := human(hold.ID, key, body, "controlled-admin")
			authorized.Store(true)
			if rec.Code != 403 || billingCalls.Load() != beforeBilling || tokenCalls.Load() != beforeToken {
				t.Fatal("permission denial crossed domains", rec.Code)
			}
			if fault == "billing-ack-lost" || fault == "unknown-crosses-deadline" {
				dropBilling.Store(true)
			}
			if fault == "token-ack-lost" {
				dropToken.Store(true)
			}
			rec = human(hold.ID, key, body, "controlled-admin")
			if fault != "none" && rec.Code != 503 {
				t.Fatal("lost ACK falsely completed", rec.Code, rec.Body.String())
			}
			if fault == "none" && rec.Code != 200 {
				t.Fatal("normal waiver failed", rec.Code, rec.Body.String())
			}
			var command usagewaiver.Command
			var raw []byte
			if err := r.pool.QueryRow(ctx, `SELECT command_snapshot FROM usage_waiver_intents WHERE usage_request_id=$1`, usageID).Scan(&raw); err != nil || json.Unmarshal(raw, &command) != nil {
				t.Fatal("original durable intent missing", err)
			}
			if command.ActorID != actor || command.AuthorizationVersion != 19 || command.Issuer != "https://controlled-waiver.invalid" || command.Subject != "operator" {
				t.Fatal("actor not frozen from authorization", command)
			}
			if fault == "unknown-crosses-deadline" {
				current, err := r.GetUsage(ctx, usageID)
				if err != nil {
					t.Fatal(err)
				}
				r.now = func() time.Time { return current.StartedAt.Add(25 * time.Hour) }
				defer func() { r.now = func() time.Time { return time.Now().UTC() } }()
				// Persisted intent must be recovered by the actual reconciler, not a new
				// key or normal Commit. Its original Billing result is already durable.
				reconciler := NewUsageReconciler(r, client, nil, time.Second)
				reconciler.now = r.now
				reconciler.runOnce(ctx)
			}
			rec = human(hold.ID, key, body, "controlled-admin")
			if rec.Code != 200 {
				t.Fatal("same command recovery failed", rec.Code, rec.Body.String())
			}
			var result usagewaiver.Result
			if json.Unmarshal(rec.Body.Bytes(), &result) != nil || !result.Matches(command) || result.Outcome != usagewaiver.Waived {
				t.Fatal("invalid real authority DTO", rec.Body.String())
			}
			current, err := r.GetUsage(ctx, usageID)
			if err != nil || current.Status != "RELEASED" || current.SettlementStatus != "RELEASED" || current.ActualMicrocredits == nil || *current.ActualMicrocredits != 0 {
				t.Fatal("Token did not converge", current, err)
			}
			var attempt string
			var input, output int64
			var provider, reference string
			if err := r.pool.QueryRow(ctx, `SELECT status,input_tokens,output_tokens,provider_id,upstream_request_id FROM usage_attempts WHERE usage_request_id=$1`, usageID).Scan(&attempt, &input, &output, &provider, &reference); err != nil || attempt != "STREAMING" || input != 2 || output != 3 || provider != "controlled" || reference != "original-provider:"+usageID.String() {
				t.Fatal("waiver rewrote provider fact", attempt, input, output, provider, reference, err)
			}
			var evidence map[string]any
			if json.Unmarshal(current.UsageSnapshot, &evidence) != nil || evidence["provider_observation"] != "original" {
				t.Fatal("original usage snapshot overwritten")
			}
			wallet, err := funds.GetWallet(ctx, walletID)
			if err != nil || wallet.PostedMicrocredits != 100 || wallet.ReservedMicrocredits != 0 {
				t.Fatal("waiver charged money", wallet, err)
			}
			var actions, journals, results int64
			if err := bp.QueryRow(ctx, `SELECT (SELECT count(*) FROM usage_hold_admin_actions WHERE hold_id=$1),(SELECT count(*) FROM journal_entries WHERE business_reference=$2),(SELECT count(*) FROM usage_waiver_results WHERE command_reference=$3)`, hold.ID, "usage:"+usageID.String(), command.Reference).Scan(&actions, &journals, &results); err != nil || actions != 1 || journals != 0 || results != 1 {
				t.Fatal("winner/audit duplication", actions, journals, results, err)
			}
			var loss int64
			if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM usage_platform_loss_reviews WHERE usage_request_id=$1 AND customer_debit_prohibited`, usageID).Scan(&loss); err != nil || fault == "unknown-crosses-deadline" && loss != 1 {
				t.Fatal("deadline no-later-debit fact missing", loss, err)
			}
			frozen, _ := json.Marshal(result)
			again := human(hold.ID, key, body, "controlled-admin")
			if again.Code != 200 {
				t.Fatal(again.Code)
			}
			var replay usagewaiver.Result
			json.Unmarshal(again.Body.Bytes(), &replay)
			got, _ := json.Marshal(replay)
			if !bytes.Equal(frozen, got) {
				t.Fatal("original authority replay changed")
			}
			beforeCalls := ordinaryMoneyCalls.Load()
			if fenced, err := r.PreserveUsageWaiverEvidence(ctx, usageID, Usage{InputTokens: 99, OutputTokens: 99}); err != nil || !fenced {
				t.Fatal("late waiver evidence did not fence", fenced, err)
			}
			service := &Service{repository: r, executionBilling: client}
			if got := service.settlePrepared(ctx, usageID, hold.ID, 99, Usage{InputTokens: 99, OutputTokens: 99}, map[string]any{}); got != "SETTLEMENT_REVIEW_REQUIRED" || ordinaryMoneyCalls.Load() != beforeCalls {
				t.Fatal("late usage reached financial writer", got)
			}
			if maximum, _, ok := service.authorizeAdditional(ctx, usageID, hold.ID, PriceVersion{InputPerMillion: 1000000, OutputPerMillion: 1000000}, Usage{InputTokens: 99, OutputTokens: 99}, 40); ok || maximum != 40 || ordinaryMoneyCalls.Load() != beforeCalls {
				t.Fatal("late usage reached ordinary Extend", maximum, ok)
			}
			// Immutable receipt and current trusted/provider facts remain exact.
			var trusted json.RawMessage
			if err := r.pool.QueryRow(ctx, `SELECT trusted_usage_snapshot FROM usage_requests WHERE id=$1`, usageID).Scan(&trusted); err != nil {
				t.Fatal(err)
			}
			var numbers Usage
			json.Unmarshal(trusted, &numbers)
			if !reflect.DeepEqual(numbers, Usage{InputTokens: 2, OutputTokens: 3}) {
				t.Fatal("late bookkeeping replaced trusted original", numbers)
			}
		})
	}
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "persisted-active-rejection-retires-only-original-claim", true: "lost-rejection-ack-retains-pending-until-original-key"}[lost], func(t *testing.T) {
			usageID, hold, walletID := seed(t, 40, time.Hour)
			if _, err := r.pool.Exec(ctx, `UPDATE usage_requests SET status='UPSTREAM_ACTIVE',execution_status='STREAMING' WHERE id=$1`, usageID); err != nil {
				t.Fatal(err)
			}
			key, body := uuid.NewString(), `{"reason":"operator while provider still active"}`
			if lost {
				dropToken.Store(true)
			}
			first := human(hold.ID, key, body, "controlled-admin")
			expected := 409
			if lost {
				expected = 503
			}
			if first.Code != expected {
				t.Fatal("incorrect initial admission result", first.Code, first.Body.String())
			}
			var decisions, claims, intents int64
			if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM usage_waiver_decisions WHERE command_snapshot->>'usage_request_id'=$1 AND admission='REJECTED'),(SELECT count(*) FROM usage_waiver_intents WHERE usage_request_id=$2)`, usageID.String(), usageID).Scan(&decisions, &intents); err != nil || decisions != 1 || intents != 0 {
				t.Fatal("rejection was not durable before ACK", decisions, intents, err)
			}
			if err := bp.QueryRow(ctx, `SELECT count(*) FROM usage_waiver_hold_claims WHERE hold_id=$1`, hold.ID).Scan(&claims); err != nil || lost && claims != 1 || !lost && claims != 0 {
				t.Fatal("unknown guessed or rejection not retired", claims, err)
			}
			if lost {
				before := tokenCalls.Load()
				blocked := human(hold.ID, uuid.NewString(), body, "controlled-admin")
				if blocked.Code != 409 || tokenCalls.Load() != before {
					t.Fatal("replacement key crossed unknown original", blocked.Code)
				}
				again := human(hold.ID, key, body, "controlled-admin")
				if again.Code != 409 {
					t.Fatal("original rejection not recovered", again.Code)
				}
			}
			// Later genuine provider review permits a fresh decision, without changing
			// the old command or pretending its original rejection was an admission.
			if _, err := r.pool.Exec(ctx, `UPDATE usage_requests SET status='REVIEW',execution_status='REVIEW' WHERE id=$1`, usageID); err != nil {
				t.Fatal(err)
			}
			old := human(hold.ID, key, body, "controlled-admin")
			if old.Code != 409 {
				t.Fatal("rejected old command reopened", old.Code)
			}
			next := human(hold.ID, uuid.NewString(), `{"reason":"new decision after provider quiesced"}`, "controlled-admin")
			if next.Code != 200 {
				t.Fatal("old rejection poisoned legitimate next decision", next.Code, next.Body.String())
			}
			var currentClaim uuid.UUID
			if err := bp.QueryRow(ctx, `SELECT command_reference FROM usage_waiver_hold_claims WHERE hold_id=$1`, hold.ID).Scan(&currentClaim); err != nil {
				t.Fatal(err)
			}
			replay := human(hold.ID, key, body, "controlled-admin")
			if replay.Code != 409 {
				t.Fatal("old rejection replay changed", replay.Code)
			}
			var after uuid.UUID
			if err := bp.QueryRow(ctx, `SELECT command_reference FROM usage_waiver_hold_claims WHERE hold_id=$1`, hold.ID).Scan(&after); err != nil || after != currentClaim {
				t.Fatal("old rejection erased newer claim", err)
			}
			wallet, err := funds.GetWallet(ctx, walletID)
			if err != nil || wallet.PostedMicrocredits != 100 || wallet.ReservedMicrocredits != 0 {
				t.Fatal("rejection/retry charged customer", wallet, err)
			}
		})
	}
	for _, priorCommit := range []bool{false, true} {
		t.Run(map[bool]string{false: "continual-no-result-known-reference-crosses-deadline", true: "prior-committed-winner-unknown-crosses-deadline"}[priorCommit], func(t *testing.T) {
			usageID, hold, walletID := seed(t, 40, time.Hour)
			key, body := uuid.NewString(), `{"reason":"explicit waiver with unavailable financial writer"}`
			unavailableBeforeBilling.Store(true)
			defer unavailableBeforeBilling.Store(false)
			response := human(hold.ID, key, body, "controlled-admin")
			if response.Code != 503 {
				t.Fatal("before-handler failure was not unknown", response.Code, response.Body.String())
			}
			var raw []byte
			if err := r.pool.QueryRow(ctx, `SELECT command_snapshot FROM usage_waiver_intents WHERE usage_request_id=$1`, usageID).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var command usagewaiver.Command
			if json.Unmarshal(raw, &command) != nil || !command.Valid() {
				t.Fatal("original admitted command missing")
			}
			// This real ordinary Commit is deliberately completed BEFORE the cutoff.
			// A later unknown retry must read that actual winner, never erase it.
			if priorCommit {
				if err := client.Commit(ctx, usageID, hold.ID, 25, Usage{InputTokens: 2, OutputTokens: 3}); err != nil {
					t.Fatal("prior real committed winner", err)
				}
			}
			originalFinancial := f05DeadlineBillingFacts(t, ctx, bp)
			current, err := r.GetUsage(ctx, usageID)
			if err != nil {
				t.Fatal(err)
			}
			frozenStarted := current.StartedAt
			clock := frozenStarted.Add(25 * time.Hour)
			r.now = func() time.Time { return clock }
			defer func() { r.now = func() time.Time { return time.Now().UTC() } }()
			worker := NewUsageReconciler(r, client, nil, time.Second)
			worker.now = r.now
			beforeOrdinary, beforeWaiver, beforeRelease := ordinaryMoneyCalls.Load(), billingCalls.Load(), ordinaryReleaseCalls.Load()
			for attempt := 0; attempt < 2; attempt++ {
				worker.runOnce(ctx)
				clock = clock.Add(time.Minute)
			}
			if ordinaryMoneyCalls.Load() != beforeOrdinary || billingCalls.Load() != beforeWaiver+2 || ordinaryReleaseCalls.Load() != beforeRelease {
				t.Fatal("deadline switched original release-only RPC", ordinaryMoneyCalls.Load()-beforeOrdinary, billingCalls.Load()-beforeWaiver)
			}
			current, err = r.GetUsage(ctx, usageID)
			if err != nil || current.Status != "REVIEW" || current.SettlementStatus != "REVIEW" || current.ActualMicrocredits != nil || !current.StartedAt.Equal(frozenStarted) {
				t.Fatal("unknown invented final state/deadline", current, err)
			}
			var factsRaw []byte
			var stopped *time.Time
			var lastError *string
			var pendingIntent, receipts, losses, deadlineIntents int64
			if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM usage_waiver_intents WHERE usage_request_id=$1),(SELECT count(*) FROM usage_waiver_receipts WHERE command_reference=$2),(SELECT count(*) FROM usage_platform_loss_reviews WHERE usage_request_id=$1),(SELECT count(*) FROM usage_convergence_evidence WHERE usage_request_id=$1 AND evidence_type='CUSTOMER_HOLD_RELEASE'),reconciliation_stopped_at,reconciliation_last_error,(SELECT command_snapshot FROM usage_waiver_intents WHERE usage_request_id=$1) FROM usage_requests WHERE id=$1`, usageID, command.Reference).Scan(&pendingIntent, &receipts, &losses, &deadlineIntents, &stopped, &lastError, &factsRaw); err != nil || pendingIntent != 1 || receipts != 0 || losses != 0 || deadlineIntents != 0 || stopped != nil || lastError == nil || *lastError != "ADMIN_WAIVER_RESULT_UNKNOWN" {
				t.Fatal("unknown forged release/loss or lost pending recovery", pendingIntent, receipts, losses, deadlineIntents, stopped, lastError, err)
			}
			if !bytes.Equal(raw, factsRaw) {
				t.Fatal("unknown changed original admitted facts")
			}
			if !reflect.DeepEqual(originalFinancial, f05DeadlineBillingFacts(t, ctx, bp)) {
				t.Fatal("before-handler unknown mutated financial owner")
			}
			beforeToken := tokenCalls.Load()
			if got := human(hold.ID, uuid.NewString(), body, "controlled-admin"); got.Code != 409 || tokenCalls.Load() != beforeToken {
				t.Fatal("unknown allowed replacement key", got.Code)
			}
			if got := human(hold.ID, key, body, "controlled-admin"); got.Code != 503 {
				t.Fatal("unknown original key falsely completed", got.Code)
			}
			service := &Service{repository: r, executionBilling: client}
			if got := service.settlePrepared(ctx, usageID, hold.ID, 99, Usage{InputTokens: 99}, map[string]any{}); got != "SETTLEMENT_REVIEW_REQUIRED" || ordinaryMoneyCalls.Load() != beforeOrdinary {
				t.Fatal("unknown late usage created new customer debit", got)
			}
			unavailableBeforeBilling.Store(false)
			clock = clock.Add(time.Minute)
			worker.runOnce(ctx)
			// Assert the actual persistent worker result BEFORE any human retry.
			current, err = r.GetUsage(ctx, usageID)
			expectedStatus, expectedActual, expectedPosted := "RELEASED", int64(0), int64(100)
			if priorCommit {
				expectedStatus, expectedActual, expectedPosted = "COMMITTED", 25, 75
			}
			if err != nil || current.Status != expectedStatus || current.ActualMicrocredits == nil || *current.ActualMicrocredits != expectedActual {
				t.Fatal("actual worker did not recover original financial winner", current, err)
			}
			wallet, err := funds.GetWallet(ctx, walletID)
			if err != nil || wallet.PostedMicrocredits != expectedPosted || wallet.ReservedMicrocredits != 0 {
				t.Fatal("recovered winner money", wallet, err)
			}
			response = human(hold.ID, key, body, "controlled-admin")
			var result usagewaiver.Result
			if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil || !result.Matches(command) {
				t.Fatal("original key recovery authority", response.Code, response.Body.String())
			}
			expectedOutcome, expectedAudit, expectedLoss := usagewaiver.Waived, int64(1), int64(1)
			if priorCommit {
				expectedOutcome, expectedAudit, expectedLoss = usagewaiver.Committed, 0, 0
			}
			if result.Outcome != expectedOutcome {
				t.Fatal("prior winner relabeled as waiver", result.Outcome)
			}
			var actions, results, claims int64
			if err := bp.QueryRow(ctx, `SELECT (SELECT count(*) FROM usage_hold_admin_actions WHERE hold_id=$1),(SELECT count(*) FROM usage_waiver_results WHERE command_reference=$2),(SELECT count(*) FROM usage_waiver_hold_claims WHERE hold_id=$1 AND command_reference=$2)`, hold.ID, command.Reference).Scan(&actions, &results, &claims); err != nil || actions != expectedAudit || results != 1 || claims != 1 {
				t.Fatal("original result/audit/claim changed", actions, results, claims, err)
			}
			if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM usage_platform_loss_reviews WHERE usage_request_id=$1`, usageID).Scan(&losses); err != nil || losses != expectedLoss {
				t.Fatal("confirmed winner loss fact", losses, err)
			}
		})
	}
	t.Run("admitted-missing-reference-waiver-recovers-before-automatic-deadline", func(t *testing.T) {
		var clock atomic.Int64
		r.now = func() time.Time {
			if value := clock.Load(); value != 0 {
				return time.Unix(0, value).UTC()
			}
			return time.Now().UTC()
		}
		defer func() { r.now = func() time.Time { return time.Now().UTC() } }()
		now := time.Now().UTC()
		original := f05DeadlineSeedThroughChat(t, ctx, r, funds, bp, client, &clock, now.Add(-3*time.Hour), now.Add(-3*time.Hour), true, false)
		key, body := uuid.NewString(), `{"reason":"explicit waiver before automatic deadline"}`
		unavailableBeforeBilling.Store(true)
		defer unavailableBeforeBilling.Store(false)
		first := human(*original.usage.HoldID, key, body, "controlled-admin")
		if first.Code != 503 {
			t.Fatal("original admitted unknown", first.Code, first.Body.String())
		}
		var commandRaw []byte
		if err := r.pool.QueryRow(ctx, `SELECT command_snapshot FROM usage_waiver_intents WHERE usage_request_id=$1`, original.usage.ID).Scan(&commandRaw); err != nil {
			t.Fatal("original admitted intent missing", err)
		}
		var command usagewaiver.Command
		if json.Unmarshal(commandRaw, &command) != nil || !command.Valid() {
			t.Fatal("invalid original intent")
		}
		beforeWaiver, beforeMoney, beforeRelease := billingCalls.Load(), ordinaryMoneyCalls.Load(), ordinaryReleaseCalls.Load()
		unavailableBeforeBilling.Store(false)
		NewUsageReconciler(r, client, nil, time.Second).runOnce(ctx)
		current, err := r.GetUsage(ctx, original.usage.ID)
		// Inspect the real worker before any human retry; the automatic !due
		// branch must not postpone an already admitted administrator command.
		if err != nil || current.Status != "RELEASED" || current.ActualMicrocredits == nil || *current.ActualMicrocredits != 0 || billingCalls.Load() != beforeWaiver+1 {
			t.Fatal("3h admitted waiver not recovered before automatic deadline", current.Status, billingCalls.Load()-beforeWaiver, err)
		}
		if ordinaryMoneyCalls.Load() != beforeMoney || ordinaryReleaseCalls.Load() != beforeRelease || original.providerCalls.Load() != 1 || !current.StartedAt.Equal(original.usage.StartedAt) {
			t.Fatal("waiver replaced original deadline or reran provider/financial operation")
		}
		var receipts, deadlineIntents, losses int64
		if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM usage_waiver_receipts WHERE command_reference=$1),(SELECT count(*) FROM usage_convergence_evidence WHERE usage_request_id=$2 AND evidence_type='CUSTOMER_HOLD_RELEASE'),(SELECT count(*) FROM usage_platform_loss_reviews WHERE usage_request_id=$2)`, command.Reference, original.usage.ID).Scan(&receipts, &deadlineIntents, &losses); err != nil || receipts != 1 || deadlineIntents != 0 || losses != 0 {
			t.Fatal("automatic deadline replaced early explicit waiver", receipts, deadlineIntents, losses, err)
		}
		wallet, err := funds.GetWallet(ctx, original.usage.WalletID)
		if err != nil || wallet.PostedMicrocredits != 1000 || wallet.ReservedMicrocredits != 0 {
			t.Fatal("early waiver conservation", wallet, err)
		}
		replay := human(*original.usage.HoldID, key, body, "controlled-admin")
		var result usagewaiver.Result
		if replay.Code != 200 || json.Unmarshal(replay.Body.Bytes(), &result) != nil || result.Outcome != usagewaiver.Waived || !result.Matches(command) {
			t.Fatal("early original-key authority", replay.Code, replay.Body.String())
		}
	})
	t.Run("original-deadline-intent-wins-before-administrator-admission", func(t *testing.T) {
		var clock atomic.Int64
		r.now = func() time.Time {
			if value := clock.Load(); value != 0 {
				return time.Unix(0, value).UTC()
			}
			return time.Now().UTC()
		}
		defer func() { r.now = func() time.Time { return time.Now().UTC() } }()
		now := time.Now().UTC()
		original := f05DeadlineSeedThroughChat(t, ctx, r, funds, bp, client, &clock, now.Add(-25*time.Hour), now.Add(-25*time.Hour), true, false)
		request, handled, err := r.PrepareMissingReferenceRelease(ctx, original.usage.ID)
		if err != nil || !handled || request == nil {
			t.Fatal("real original deadline intent", handled, err)
		}
		key, body := uuid.NewString(), `{"reason":"administrator after original deadline intent"}`
		response := human(*original.usage.HoldID, key, body, "controlled-admin")
		if response.Code != 409 {
			t.Fatal("deadline decision did not retain priority", response.Code, response.Body.String())
		}
		var rejected, intents, claims int64
		if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM usage_waiver_decisions WHERE command_snapshot->>'usage_request_id'=$1 AND admission='REJECTED' AND decision_snapshot->>'reason_code'='DEADLINE_RELEASE_OWNS_RECOVERY'),(SELECT count(*) FROM usage_waiver_intents WHERE usage_request_id=$2)`, original.usage.ID.String(), original.usage.ID).Scan(&rejected, &intents); err != nil || rejected != 1 || intents != 0 {
			t.Fatal("deadline rejection authority", rejected, intents, err)
		}
		if err := bp.QueryRow(ctx, `SELECT count(*) FROM usage_waiver_hold_claims WHERE hold_id=$1`, original.usage.HoldID).Scan(&claims); err != nil || claims != 0 {
			t.Fatal("definitive deadline rejection poisoned original release", claims, err)
		}
		NewUsageReconciler(r, client, nil, time.Second).runOnce(ctx)
		f05DeadlineReleased(t, r, funds, bp, ctx, original)
		replay := human(*original.usage.HoldID, key, body, "controlled-admin")
		if replay.Code != 409 {
			t.Fatal("deadline-rejected old key reopened", replay.Code, replay.Body.String())
		}
		var actions int64
		if err := bp.QueryRow(ctx, `SELECT count(*) FROM usage_hold_admin_actions WHERE hold_id=$1`, original.usage.HoldID).Scan(&actions); err != nil || actions != 0 {
			t.Fatal("deadline release fabricated admin waiver", actions, err)
		}
	})
	t.Run("wrong-workload-and-unprepared-reference-reject-before-effects", func(t *testing.T) {
		for _, target := range []struct{ url, path string }{{billingHTTP.URL, "/internal/v1/holds/waiver"}, {tokenHTTP.URL, "/internal/v1/usage-waivers"}} {
			for _, subject := range []string{"", "wrong"} {
				request, _ := http.NewRequestWithContext(ctx, http.MethodPost, target.url+target.path, strings.NewReader(`{}`))
				request.Header.Set("Content-Type", "application/json")
				if subject != "" {
					request.Header.Set("Authorization", "Bearer "+signed(subject))
				}
				response, err := http.DefaultClient.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				io.Copy(io.Discard, response.Body)
				response.Body.Close()
				expected := 403
				if subject == "" {
					expected = 401
				}
				if response.StatusCode != expected {
					t.Fatal("workload was not scoped", target.path, response.StatusCode)
				}
			}
		}
		for _, target := range []struct{ url, path, subject string }{{billingHTTP.URL, "/internal/v1/holds/waiver", "token"}, {tokenHTTP.URL, "/internal/v1/usage-waivers", "billing"}} {
			request, _ := http.NewRequestWithContext(ctx, http.MethodPost, target.url+target.path, strings.NewReader(`{"forged":"unknown"}`))
			request.Header.Set("Authorization", "Bearer "+signed(target.subject))
			request.Header.Set("Content-Type", "application/json")
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode != 400 {
				t.Fatal("malformed workload command returned success", target.path, response.StatusCode)
			}
		}
		if _, err := funds.ExecuteUsageWaiver(ctx, usagewaiver.ExecuteRequest{Reference: uuid.New(), Digest: strings.Repeat("a", 64)}); !errors.Is(err, billing.ErrNotFound) {
			t.Fatal("unprepared command executed", err)
		}
	})
}
