package token

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/holdexpiry"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type f04LocalExpiryClient struct{ service *Service }

func (c f04LocalExpiryClient) FenceExpiredHold(ctx context.Context, i billing.ExpiredHoldIdentity) (billing.ExpiredHoldProof, error) {
	return c.service.recoverExpiredHold(ctx, i)
}

type f04ChangedExpiryStore struct{ *PostgresRepository }

func (s f04ChangedExpiryStore) GetUsage(ctx context.Context, id uuid.UUID) (UsageRequest, error) {
	if err := s.MarkReview(ctx, id, "controlled-concurrent-review", nil); err != nil {
		return UsageRequest{}, err
	}
	return s.PostgresRepository.GetUsage(ctx, id)
}

func TestF04ExpiredHoldCrossServiceLostReleaseACK(t *testing.T) {
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
	if _, err = funds.Credit(ctx, wallet.ID, "expiry-"+user.String(), 1000, false); err != nil {
		t.Fatal(err)
	}
	usage := retainedCancellationUsage(t, r, ctx, user, wallet.ID, nil, "HOLD_RESERVED")
	hold, err := funds.Reserve(ctx, usage, wallet.ID, 100, nil, time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE usage_holds SET expires_at=clock_timestamp()-interval '1 minute' WHERE id=$1`, hold.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = r.pool.Exec(ctx, `UPDATE usage_requests SET hold_id=$2,status='RESERVED',settlement_status='HOLD_ACTIVE',cancel_status='NOT_REQUESTED' WHERE id=$1`, usage, hold.ID); err != nil {
		t.Fatal(err)
	}
	var loseACK atomic.Bool
	loseACK.Store(true)
	handler := billing.NewService(funds).Handler("controlled-service")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/internal/v1/holds/release" && loseACK.Swap(false) {
			receipt := httptest.NewRecorder()
			handler.ServeHTTP(receipt, request)
			if receipt.Code != 200 {
				t.Error("release did not commit", receipt.Code)
			}
			w.WriteHeader(503)
			return
		}
		handler.ServeHTTP(w, request)
	}))
	defer server.Close()
	client, err := NewHTTPBillingClient(server.URL, "controlled-service")
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{repository: r, billing: client, cancellationBilling: client, cancellationStore: r}
	worker := billing.NewExpiredHoldRecoveryWorker(funds, f04LocalExpiryClient{service}, time.Minute)
	if err = worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	current, err := r.GetUsage(ctx, usage)
	if err != nil || current.CancelStatus == "LOCAL_CONFIRMED" || current.ExecutionStatus != "REVIEW" {
		t.Fatal("lost ACK falsely confirmed", current, err)
	}
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
		t.Fatal("canonical recovery", current, err)
	}
	wallet, err = funds.GetWallet(ctx, wallet.ID)
	if err != nil || wallet.ReservedMicrocredits != 0 || wallet.AvailableMicrocredits != 1000 {
		t.Fatal("release changed balance", wallet, err)
	}
	if err = pool.QueryRow(ctx, `SELECT state FROM usage_hold_expiry_recovery WHERE hold_id=$1`, hold.ID).Scan(&state); err != nil || state != "COMPLETED" {
		t.Fatal(state, err)
	}
}

func TestF04ExpiredHoldDoesNotReturnUnconfirmedProof(t *testing.T) {
	r, ctx := retainedManagementTokenRepository(t)
	wallet, hold := uuid.New(), uuid.New()
	usage := retainedCancellationUsage(t, r, ctx, uuid.New(), wallet, &hold, "HOLD_RESERVED")
	service := &Service{repository: r, cancellationStore: f04ChangedExpiryStore{r}, cancellationBilling: &f04NeverRelease{}}
	proof, err := service.recoverExpiredHold(ctx, holdexpiry.Identity{UsageRequestID: usage, HoldID: hold, WalletID: wallet, MaxMicrocredits: 100, ExpiresAt: time.Now().UTC().Add(-time.Minute)})
	if err == nil || proof.Status == "LOCAL_CONFIRMED" {
		t.Fatal("unconfirmed concurrent state returned proof", proof, err)
	}
}

type f04NeverRelease struct{}

func (*f04NeverRelease) Reserve(context.Context, uuid.UUID, uuid.UUID, int64, uuid.UUID) (BillingHold, error) {
	return BillingHold{}, errors.New("must not reserve")
}
func (*f04NeverRelease) Release(context.Context, uuid.UUID, uuid.UUID, string) error {
	return errors.New("must not release")
}

func TestF04ExpiredHoldFenceUsesPersistedCanonicalFacts(t *testing.T) {
	r, ctx := retainedManagementTokenRepository(t)
	for _, scenario := range []string{"valid", "intent", "legacy", "mismatch", "streaming", "received", "not-expired"} {
		t.Run(scenario, func(t *testing.T) {
			hold, wallet := uuid.New(), uuid.New()
			state := "HOLD_RESERVED"
			if scenario == "streaming" {
				state = "STREAMING"
			}
			if scenario == "received" {
				state = "RECEIVED"
			}
			usage := retainedCancellationUsage(t, r, ctx, uuid.New(), wallet, &hold, state)
			identity := holdexpiry.Identity{UsageRequestID: usage, HoldID: hold, WalletID: wallet, MaxMicrocredits: 100, ExpiresAt: time.Now().UTC().Add(-time.Minute)}
			if scenario == "legacy" {
				if _, e := r.pool.Exec(ctx, `UPDATE usage_requests SET canonicalization_version=0 WHERE id=$1`, usage); e != nil {
					t.Fatal(e)
				}
			}
			if scenario == "intent" {
				if _, e := r.pool.Exec(ctx, `UPDATE usage_requests SET cancel_status='NOT_REQUESTED' WHERE id=$1`, usage); e != nil {
					t.Fatal(e)
				}
				if _, e := r.pool.Exec(ctx, `INSERT INTO usage_dispatch_intents(usage_request_id,upstream_idempotency_key,created_at) VALUES($1,'controlled-intent',clock_timestamp())`, usage); e != nil {
					t.Fatal(e)
				}
			}
			if scenario == "mismatch" {
				identity.MaxMicrocredits++
			}
			if scenario == "not-expired" {
				identity.ExpiresAt = time.Now().UTC().Add(time.Hour)
			}
			proof, err := r.fenceExpiredHold(ctx, identity)
			if scenario != "valid" {
				if err == nil {
					t.Fatal("unproven reservation accepted", scenario)
				}
				var count int
				if e := r.pool.QueryRow(ctx, `SELECT count(*) FROM usage_cancellation_evidence WHERE usage_request_id=$1 AND adapter_version='hold-expiry-v1'`, usage).Scan(&count); e != nil || count != 0 {
					t.Fatal("rejected request persisted proof", count, e)
				}
				return
			}
			if err != nil || proof.ProofID == uuid.Nil {
				t.Fatal("valid fence", err)
			}
			same, err := r.fenceExpiredHold(ctx, identity)
			if err != nil || same.ProofID != proof.ProofID {
				t.Fatal("fence replay", err)
			}
			// The installed database trigger enforces the shared dispatch gate.
			tx, err := r.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			var cancel string
			if err = tx.QueryRow(ctx, `SELECT cancel_status FROM usage_requests WHERE id=$1 FOR UPDATE`, usage).Scan(&cancel); err != nil || cancel != "CANCEL_REQUESTED" {
				t.Fatal("dispatch not fenced", cancel, err)
			}
			tx.Rollback(ctx)
			if _, e := r.pool.Exec(ctx, `INSERT INTO usage_dispatch_intents(usage_request_id,upstream_idempotency_key,created_at) VALUES($1,'rejected-after-proof',clock_timestamp())`, usage); e == nil {
				t.Fatal("proof allowed a later dispatch")
			}
			if err = r.MarkReview(ctx, usage, "cancel_hold_recovery_unconfirmed", map[string]any{"customer_hold_release_unconfirmed": true}); err != nil {
				t.Fatal(err)
			}
			again, err := r.fenceExpiredHold(ctx, identity)
			if err != nil || again.ProofID != proof.ProofID {
				t.Fatal("lost release ACK not recoverable", err)
			}
		})
	}
}

func TestF04ExpiredHoldIntentWinsSharedDatabaseGate(t *testing.T) {
	r, ctx := retainedManagementTokenRepository(t)
	started := time.Now()
	stage := "intent-setup"
	t.Cleanup(func() {
		t.Logf("F04 shared gate: stage=%s elapsed_ms=%d", stage, time.Since(started).Milliseconds())
		if !t.Failed() {
			return
		}
		diagnostic, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		var locks string
		// Fixed state only: no SQL, connection URL, credentials or row facts.
		if err := r.pool.QueryRow(diagnostic, `SELECT COALESCE(jsonb_agg(jsonb_build_object(
 'state',state,'wait_type',wait_event_type,'wait_event',wait_event,
 'blockers',cardinality(pg_blocking_pids(pid))) ORDER BY pid),'[]'::jsonb)::text
 FROM pg_stat_activity WHERE datname=current_database()`).Scan(&locks); err != nil {
			t.Log("F04 shared gate lock diagnostics unavailable")
		} else {
			t.Log("F04 shared gate lock diagnostics: " + locks)
		}
	})
	hold, wallet := uuid.New(), uuid.New()
	usage := retainedCancellationUsage(t, r, ctx, uuid.New(), wallet, &hold, "HOLD_RESERVED")
	if _, err := r.pool.Exec(ctx, `UPDATE usage_requests SET cancel_status='NOT_REQUESTED' WHERE id=$1`, usage); err != nil {
		t.Fatal(err)
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO usage_dispatch_intents(usage_request_id,upstream_idempotency_key,created_at) VALUES($1,'committing-intent',clock_timestamp())`, usage); err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	deadline, _ := bounded.Deadline()
	result := make(chan f04ProofOutcome, 1)
	go func() {
		_, e := r.fenceExpiredHold(bounded, holdexpiry.Identity{UsageRequestID: usage, HoldID: hold, WalletID: wallet, MaxMicrocredits: 100, ExpiresAt: time.Now().UTC().Add(-time.Minute)})
		result <- f04ProofOutcome{err: e, completedAt: time.Now()}
	}()
	stage = "await-proof-row-lock"
	readyCtx, readyCancel := context.WithTimeout(ctx, 2*time.Second)
	defer readyCancel()
	for {
		var waiting bool
		if err = r.pool.QueryRow(readyCtx, `SELECT EXISTS(
			SELECT 1 FROM pg_stat_activity
			WHERE datname=current_database() AND wait_event='transactionid'
			  AND query LIKE '%FROM usage_requests%FOR UPDATE%'
		)`).Scan(&waiting); err != nil {
			t.Fatal("inspect proof lock wait", err)
		}
		if waiting {
			t.Logf("F04 shared gate: row_wait_observed_ms=%d", time.Since(started).Milliseconds())
			break
		}
		select {
		case e := <-result:
			t.Fatal("proof did not wait for in-flight intent", e.err)
		case <-readyCtx.Done():
			t.Fatal("proof did not reach the shared lock before deadline")
		case <-time.After(10 * time.Millisecond):
		}
	}
	stage = "commit-intent"
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("F04 shared gate: commit_ms=%d proof_remaining_ms=%d", time.Since(started).Milliseconds(), time.Until(deadline).Milliseconds())
	stage = "await-proof-denial"
	outcome, completed := f04AwaitProofOutcome(bounded, result)
	if !completed {
		t.Fatal("proof remained blocked after commit")
	}
	if !f04TimelyProofDenial(outcome, deadline) {
		t.Fatal("committed intent must deny proof before the original deadline", outcome.err)
	}
	var preserved bool
	if err := r.pool.QueryRow(ctx, `SELECT cancel_status='NOT_REQUESTED'
 AND EXISTS(SELECT 1 FROM usage_dispatch_intents WHERE usage_request_id=$1 AND upstream_idempotency_key='committing-intent')
 AND NOT EXISTS(SELECT 1 FROM usage_cancellation_evidence WHERE usage_request_id=$1 AND adapter_version='hold-expiry-v1' AND evidence_type='CANCEL_REQUESTED')
 FROM usage_requests WHERE id=$1`, usage).Scan(&preserved); err != nil || !preserved {
		t.Fatal("denial changed the committed intent or produced cancellation proof", err)
	}
	stage = "proof-denied"
	t.Logf("F04 shared gate: proof_return_ms=%d", outcome.completedAt.Sub(started).Milliseconds())
}

type f04ProofOutcome struct {
	err         error
	completedAt time.Time
}

func f04AwaitProofOutcome(ctx context.Context, result <-chan f04ProofOutcome) (f04ProofOutcome, bool) {
	select {
	case outcome := <-result:
		return outcome, true
	case <-ctx.Done():
		// A scheduled reader may see both ready. Preserve an already recorded
		// result; its completion timestamp still has to meet the same deadline.
		select {
		case outcome := <-result:
			return outcome, true
		default:
			return f04ProofOutcome{}, false
		}
	}
}

func f04TimelyProofDenial(outcome f04ProofOutcome, deadline time.Time) bool {
	return errors.Is(outcome.err, ErrForbidden) && !outcome.completedAt.IsZero() && outcome.completedAt.Before(deadline)
}

func TestF04ProofResultAndDeadlineReadyTogether(t *testing.T) {
	deadline := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, test := range []struct {
		name string
		err  error
		at   time.Time
		want bool
	}{
		{"timely-denial", ErrForbidden, deadline.Add(-time.Nanosecond), true},
		{"deadline-is-not-denial", context.DeadlineExceeded, deadline.Add(-time.Nanosecond), false},
		{"database-error-is-not-denial", errors.New("controlled database failure"), deadline.Add(-time.Nanosecond), false},
		{"success-is-not-denial", nil, deadline.Add(-time.Nanosecond), false},
		{"late-denial", ErrForbidden, deadline, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := make(chan f04ProofOutcome, 1)
			result <- f04ProofOutcome{err: test.err, completedAt: test.at}
			outcome, completed := f04AwaitProofOutcome(ctx, result)
			if !completed || f04TimelyProofDenial(outcome, deadline) != test.want {
				t.Fatal("result/deadline arbitration changed the observed denial")
			}
		})
	}
	if _, completed := f04AwaitProofOutcome(ctx, make(chan f04ProofOutcome, 1)); completed {
		t.Fatal("missing result was accepted after cancellation")
	}
}

func TestF04CatalogAbsencePreservesApprovedLifecycle(t *testing.T) {
	r, ctx := retainedManagementTokenRepository(t)
	actor := uuid.New()
	source := []DiscoveredModel{{UpstreamModelRef: "controlled-missing", DisplayName: "Controlled"}}
	if _, _, err := r.SyncCatalog(ctx, "controlled-f04", "v1", source, 32000, 1024, actor); err != nil {
		t.Fatal(err)
	}
	_, models, err := r.ListCatalog(ctx)
	if err != nil || len(models) != 1 {
		t.Fatal("catalog fixture", err, len(models))
	}
	id := models[0].ModelID
	// Existing approved metadata is preserved; this fixture does not authorize
	// a new lifecycle command or rewrite immutable catalog history.
	for n := 1; n <= 3; n++ {
		if _, _, err = r.SyncCatalog(ctx, "controlled-f04", "v1", nil, 32000, 1024, actor); err != nil {
			t.Fatal(err)
		}
		_, models, err = r.ListCatalog(ctx)
		if err != nil {
			t.Fatal(err)
		}
		m := models[0]
		if m.ModelID != id || m.SourceAvailable || m.SourceMissingConfirmations != n || m.SourceMissingSince == nil || m.LifecycleStatus != "PUBLISHED" || m.DeprecationEffectiveAt != nil || m.NewSessionCutoffAt != nil || m.ContinuationCutoffAt != nil {
			t.Fatalf("absence performed lifecycle decision: %#v", m)
		}
		if _, err = r.ResolveCatalogModel(ctx, id.String(), "NEW", time.Now()); err == nil {
			t.Fatal("unavailable source became usable")
		}
	}
	if _, _, err = r.SyncCatalog(ctx, "controlled-f04", "v1", source, 32000, 1024, actor); err != nil {
		t.Fatal(err)
	}
	_, models, err = r.ListCatalog(ctx)
	if err != nil || !models[0].SourceAvailable || models[0].LifecycleStatus != "PUBLISHED" || models[0].SourceMissingConfirmations != 0 {
		t.Fatal("source recovery", err)
	}
	// Install explicit lifecycle policy through the existing domain transition,
	// rather than rewriting an immutable catalog entry with a fixture UPDATE.
	effective := time.Now().UTC().Truncate(time.Microsecond).Add(time.Hour)
	newCutoff := effective
	continueCutoff := effective.Add(2 * time.Hour)
	plan, _, err := r.CreateLifecyclePlan(ctx, id, LifecyclePlanInput{ExpectedCatalogVersion: models[0].CatalogVersion, TargetLifecycle: "DEPRECATED", EffectiveAt: effective, ExistingSessionCutoffAt: &continueCutoff, Reason: "Controlled explicit lifecycle policy"}, actor, uuid.NewString())
	if err != nil {
		t.Fatal("explicit lifecycle plan", err)
	}
	evidence, err := r.ModelLifecycleImpact(ctx, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	critical, _ := json.Marshal(lifecyclePlanCritical{ModelID: id, CatalogVersion: models[0].CatalogVersion, CurrentLifecycle: "PUBLISHED", TargetLifecycle: "DEPRECATED", LifecyclePlanVersion: plan.PlanVersion, EffectiveAt: effective, ExistingSessionCutoffAt: &continueCutoff, PolicyVersion: 2, RouteSetFingerprint: evidence.RouteSetFingerprint, ProvisioningSetFingerprint: evidence.ProvisioningSetFingerprint, InflightRequestSetFingerprint: evidence.InflightSetFingerprint})
	// This owned fixture supplies the reviewed approval facts; the actual
	// lifecycle claim/apply performs the immutable publication and audit.
	if _, err = r.pool.Exec(ctx, `UPDATE model_lifecycle_plans SET status='SCHEDULED',approval_request_id=$2,approval_command_fingerprint=$3,critical_resource_versions=$4,financial_snapshot='{}' WHERE id=$1`, plan.ID, uuid.New(), strings.Repeat("a", 64), critical); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := r.claimDueLifecyclePlan(ctx, "controlled-manual-deprecation", &effective)
	if err != nil || !ok || claimed.ID != plan.ID {
		t.Fatal("explicit lifecycle claim", err)
	}
	if err = r.ApplyClaimedLifecyclePlan(ctx, plan.ID, "controlled-manual-deprecation", nil); err != nil {
		t.Fatal("explicit lifecycle application", err)
	}
	for _, observed := range [][]DiscoveredModel{nil, nil, source} {
		if _, _, err = r.SyncCatalog(ctx, "controlled-f04", "v1", observed, 32000, 1024, actor); err != nil {
			t.Fatal(err)
		}
		_, models, err = r.ListCatalog(ctx)
		if err != nil {
			t.Fatal(err)
		}
		m := models[0]
		if m.LifecycleStatus != "DEPRECATED" || m.DeprecationEffectiveAt == nil || !m.DeprecationEffectiveAt.Equal(effective) || m.NewSessionCutoffAt == nil || !m.NewSessionCutoffAt.Equal(newCutoff) || m.ContinuationCutoffAt == nil || !m.ContinuationCutoffAt.Equal(continueCutoff) {
			t.Fatal("source observation rewrote explicit lifecycle", m)
		}
	}
	var accidental int
	if err = r.pool.QueryRow(ctx, `SELECT count(*) FROM model_catalog_audit WHERE model_id=$1 AND action='MODEL_SOURCE_MISSING_DEPRECATED'`, id).Scan(&accidental); err != nil || accidental != 0 {
		t.Fatal("observation mislabeled lifecycle authorization", accidental, err)
	}
}
