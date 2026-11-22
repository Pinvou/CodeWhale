package token

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Uses the existing isolated public-migration Token fixture. All dispatch,
// renewal, recovery and replay writes below are the production methods. No
// live provider or Billing operation is executed; time advances atomically.
func TestF05DispatchLeaseLifecycle(t *testing.T) {
	r, ctx := retainedManagementTokenRepository(t)
	base := time.Now().UTC().Truncate(time.Microsecond)
	var clock atomic.Int64
	clock.Store(base.UnixNano())
	r.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	setClock := func(elapsed time.Duration) { clock.Store(base.Add(elapsed).UnixNano()) }
	seed := func(t *testing.T) (uuid.UUID, DispatchEnvelope) {
		t.Helper()
		setClock(0)
		hold := uuid.New()
		id := retainedCancellationUsage(t, r, ctx, uuid.New(), uuid.New(), &hold, "HOLD_RESERVED")
		if _, err := r.pool.Exec(ctx, `UPDATE usage_requests SET status='RESERVED',settlement_status='HOLD_ACTIVE',cancel_status='NOT_REQUESTED',cancel_requested_at=NULL WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
		envelope := DispatchEnvelope{BaseURL: "https://provider.invalid", Provider: "controlled", Credential: []byte("controlled-lease-secret"), RequestBody: []byte(`{"model":"model"}`), Price: PriceVersion{ID: uuid.New()}, MaximumCost: 100}
		if err := r.PersistDispatchIntent(ctx, id, envelope); err != nil {
			t.Fatal(err)
		}
		actual, acquired, err := r.AcquireDispatch(ctx, id, "actual-owner", dispatchLeaseDuration)
		if err != nil || !acquired || actual.leaseOwner != "actual-owner" {
			t.Fatal("production one-shot acquisition", acquired, actual.leaseOwner, err)
		}
		if err := r.MarkUpstream(ctx, id); err != nil {
			t.Fatal(err)
		}
		return id, actual
	}
	until := func(t *testing.T, id uuid.UUID) time.Time {
		t.Helper()
		var value time.Time
		if err := r.pool.QueryRow(ctx, `SELECT worker_lease_until FROM usage_dispatch_intents WHERE usage_request_id=$1`, id).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	result := ReplayResult{Kind: "JSON", HTTPStatus: http.StatusOK, ContentType: "application/json", Payload: []byte(`{"choices":[]}`), ResponseReceived: true, ResponseDisposition: responseDispositionUsageEvidenceRequired}
	ownerContext := withDispatchLeaseOwner(ctx, "actual-owner")
	assertNoRedispatch := func(t *testing.T, id uuid.UUID) {
		t.Helper()
		for _, owner := range []string{"actual-owner", "different-owner"} {
			envelope, acquired, err := r.AcquireDispatch(ctx, id, owner, dispatchLeaseDuration)
			if err != nil || acquired || len(envelope.Credential) != 0 || len(envelope.RequestBody) != 0 {
				t.Fatal("acquired intent was redispatched", owner, acquired, err)
			}
		}
		var originalOwner string
		var erased bool
		if err := r.pool.QueryRow(ctx, `SELECT worker_lease_owner,encrypted_envelope IS NULL AND key_id IS NULL AND envelope_integrity IS NULL FROM usage_dispatch_intents WHERE usage_request_id=$1`, id).Scan(&originalOwner, &erased); err != nil || originalOwner != "actual-owner" || !erased {
			t.Fatal("dispatch owner or one-shot envelope changed", originalOwner, erased, err)
		}
	}
	assertActive := func(t *testing.T, id uuid.UUID, execution string) {
		t.Helper()
		current, err := r.GetUsage(ctx, id)
		if err != nil || current.ExecutionStatus != execution || current.Status != "UPSTREAM_ACTIVE" || current.ActualMicrocredits != nil || current.SettlementStatus != "HOLD_ACTIVE" {
			t.Fatal("live dispatch or original reservation was changed", current, err)
		}
	}
	t.Run("real-service-renewal-covers-two-hours-and-replay-closure", func(t *testing.T) {
		id, envelope := seed(t)
		defer clear(envelope.Credential)
		ticks := make(chan time.Time)
		var cancelled atomic.Int64
		service := &Service{repository: r}
		failed, stop, err := service.renewDispatchLease(ownerContext, func() { cancelled.Add(1) }, id, envelope.leaseOwner, ticks)
		if err != nil {
			t.Fatal(err)
		}
		defer stop()
		// At 12 minutes the original implementation already called MarkReview.
		// At 124 minutes the upstream budget is over but replay closure may still
		// own the dispatch. This is the same injected-channel service loop used
		// by the production 4-minute ticker, not a constant-only assertion.
		for elapsed := 4 * time.Minute; elapsed <= 124*time.Minute; elapsed += 4 * time.Minute {
			setClock(elapsed)
			select {
			case ticks <- r.now():
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			f05WaitDispatchLeaseFact(t, ctx, func() bool { return until(t, id).Equal(base.Add(elapsed + 10*time.Minute)) })
			if err := r.RecoverUncertainDispatches(ctx); err != nil {
				t.Fatal(err)
			}
			assertActive(t, id, "UPSTREAM_DISPATCHING")
		}
		assertNoRedispatch(t, id)
		if cancelled.Load() != 0 {
			t.Fatal("healthy dispatch was cancelled")
		}
		select {
		case <-failed:
			t.Fatal("healthy dispatch lease failed")
		default:
		}
		if err := r.SaveReplayResult(ownerContext, id, result); err != nil {
			t.Fatal("current owner could not persist complete response", err)
		}
		stop()
		setClock(140 * time.Minute)
		if err := r.RecoverUncertainDispatches(ctx); err != nil {
			t.Fatal(err)
		}
		replay, err := r.LoadReplay(ctx, id, 0)
		if err != nil || replay.Result == nil || replay.Usage.ExecutionStatus != "SETTLEMENT_PENDING" || replay.Usage.ErrorClass == nil || *replay.Usage.ErrorClass != "usage_evidence_required" {
			t.Fatal("expired old lease downgraded durable response", replay, err)
		}
		assertNoRedispatch(t, id)
	})
	t.Run("expired-or-foreign-owner-cannot-resurrect-or-persist", func(t *testing.T) {
		id, envelope := seed(t)
		defer clear(envelope.Credential)
		originalUntil := until(t, id)
		if active, err := r.RenewDispatchLease(ctx, id, "foreign-owner"); active || !errors.Is(err, ErrForbidden) {
			t.Fatal("foreign owner renewed", active, err)
		}
		if err := r.SaveReplayResult(withDispatchLeaseOwner(ctx, "foreign-owner"), id, result); !errors.Is(err, ErrForbidden) {
			t.Fatal("foreign owner published response", err)
		}
		if !until(t, id).Equal(originalUntil) {
			t.Fatal("foreign owner moved deadline")
		}
		setClock(10 * time.Minute)
		if active, err := r.RenewDispatchLease(ctx, id, envelope.leaseOwner); active || !errors.Is(err, ErrForbidden) {
			t.Fatal("expired owner resurrected lease", active, err)
		}
		if err := r.SaveReplayResult(ownerContext, id, result); !errors.Is(err, ErrForbidden) {
			t.Fatal("expired owner published response before recovery", err)
		}
		if _, err := r.AppendReplayEvent(ownerContext, id, "DELTA", []byte(`{"text":"late"}`)); !errors.Is(err, ErrForbidden) {
			t.Fatal("expired owner published a stream event", err)
		}
		assertActive(t, id, "UPSTREAM_DISPATCHING")
		if err := r.RecoverUncertainDispatches(ctx); err != nil {
			t.Fatal(err)
		}
		current, err := r.GetUsage(ctx, id)
		if err != nil || current.ExecutionStatus != "REVIEW" || current.ActualMicrocredits != nil || current.HoldID == nil {
			t.Fatal("expired dispatch did not retain original financial identity", current, err)
		}
		if err := r.SaveReplayResult(ownerContext, id, result); !errors.Is(err, ErrForbidden) {
			t.Fatal("late result replaced recovered execution", err)
		}
		if err := r.MarkReview(ownerContext, id, "upstream_transport_unknown", map[string]any{"late_owner": true}); !errors.Is(err, ErrForbidden) {
			t.Fatal("expired owner replaced the worker's review", err)
		}
		var bodies, events, uncertainty int
		if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM usage_replay_results WHERE usage_request_id=$1),(SELECT count(*) FROM usage_stream_events WHERE usage_request_id=$1),(SELECT count(*) FROM usage_convergence_evidence WHERE usage_request_id=$1 AND evidence_type='UNCERTAINTY' AND evidence_key='dispatch_result_unknown')`, id).Scan(&bodies, &events, &uncertainty); err != nil || bodies != 0 || events != 0 || uncertainty != 1 {
			t.Fatal("late owner published content or duplicated recovery", bodies, events, uncertainty, err)
		}
		if err := r.RecoverUncertainDispatches(ctx); err != nil {
			t.Fatal(err)
		}
		assertNoRedispatch(t, id)
	})
	t.Run("dead-stream-is-reviewed-once-without-redispatch", func(t *testing.T) {
		id, envelope := seed(t)
		defer clear(envelope.Credential)
		if _, err := r.AppendReplayEvent(ownerContext, id, "DELTA", []byte(`{"text":"before-crash"}`)); err != nil {
			t.Fatal(err)
		}
		assertActive(t, id, "STREAMING")
		setClock(11 * time.Minute)
		for i := 0; i < 2; i++ {
			if err := r.RecoverUncertainDispatches(ctx); err != nil {
				t.Fatal(err)
			}
		}
		current, err := r.GetUsage(ctx, id)
		if err != nil || current.ExecutionStatus != "REVIEW" || current.LastPersistedSequence != 1 {
			t.Fatal("dead stream did not retain evidence", current, err)
		}
		assertNoRedispatch(t, id)
	})
	t.Run("renewal-loop-loses-owner-and-cancels-inflight-only-once", func(t *testing.T) {
		id, envelope := seed(t)
		defer clear(envelope.Credential)
		ticks := make(chan time.Time)
		cancelled := make(chan struct{}, 1)
		service := &Service{repository: r}
		failed, stop, err := service.renewDispatchLease(ownerContext, func() { cancelled <- struct{}{} }, id, envelope.leaseOwner, ticks)
		if err != nil {
			t.Fatal(err)
		}
		defer stop()
		setClock(11 * time.Minute)
		ticks <- r.now()
		select {
		case <-failed:
		case <-ctx.Done():
			t.Fatal("expired lease did not fail", ctx.Err())
		}
		select {
		case <-cancelled:
		case <-ctx.Done():
			t.Fatal("inflight owner was not cancelled")
		}
		if err := r.RecoverUncertainDispatches(ctx); err != nil {
			t.Fatal(err)
		}
		assertNoRedispatch(t, id)
	})
	t.Run("recovery-first-fails-loop-and-cancels-blocked-provider-once", func(t *testing.T) {
		id, envelope := seed(t)
		defer clear(envelope.Credential)
		started := make(chan struct{})
		provider := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
			// Consume the POST body so the server can observe the client's
			// connection cancellation while deliberately withholding a response.
			_, _ = io.Copy(io.Discard, request.Body)
			request.Body.Close()
			close(started)
			<-request.Context().Done()
		}))
		defer provider.Close()
		upstreamContext, cancelUpstream := context.WithCancel(ctx)
		defer cancelUpstream()
		var cancellations atomic.Int64
		ticks := make(chan time.Time)
		service := &Service{repository: r}
		failed, stop, err := service.renewDispatchLease(ownerContext, func() {
			cancellations.Add(1)
			cancelUpstream()
		}, id, envelope.leaseOwner, ticks)
		if err != nil {
			t.Fatal(err)
		}
		defer stop()
		upstreamEnded := make(chan error, 1)
		go func() {
			response, err := NewHTTPUpstreamClient().Do(upstreamContext, provider.URL, envelope.Credential, envelope.RequestBody, id)
			if response != nil {
				response.Body.Close()
			}
			upstreamEnded <- err
		}()
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("actual provider did not block", ctx.Err())
		}
		// This is the opposite order from the predecessor's expiry test:
		// the real worker moves execution to REVIEW before the next renew tick.
		setClock(11 * time.Minute)
		if err := r.RecoverUncertainDispatches(ctx); err != nil {
			t.Fatal(err)
		}
		current, err := r.GetUsage(ctx, id)
		if err != nil || current.ExecutionStatus != "REVIEW" {
			t.Fatal("actual recovery did not win", current, err)
		}
		ticks <- r.now()
		select {
		case <-failed:
		case <-ctx.Done():
			t.Fatal("recovered dispatch stopped quietly instead of failing", ctx.Err())
		}
		select {
		case err := <-upstreamEnded:
			if !errors.Is(err, context.Canceled) {
				t.Fatal("blocked production HTTP client was not cancelled", err)
			}
		case <-ctx.Done():
			t.Fatal("blocked provider continued after recovery", ctx.Err())
		}
		stop()
		if cancellations.Load() != 1 {
			t.Fatal("lost-owner cancellation was not exactly once", cancellations.Load())
		}
		if err := r.SaveReplayResult(ownerContext, id, result); !errors.Is(err, ErrForbidden) {
			t.Fatal("cancelled old owner published late replay", err)
		}
		assertNoRedispatch(t, id)
	})
	t.Run("save-first-stops-loop-without-cancelling-completed-response", func(t *testing.T) {
		id, envelope := seed(t)
		defer clear(envelope.Credential)
		observed := &f05ObservedLeaseRepository{PostgresRepository: r, calls: make(chan f05LeaseRenewalObservation, 2)}
		ticks := make(chan time.Time)
		var cancellations atomic.Int64
		service := &Service{repository: observed}
		failed, stop, err := service.renewDispatchLease(ownerContext, func() { cancellations.Add(1) }, id, envelope.leaseOwner, ticks)
		if err != nil {
			t.Fatal(err)
		}
		defer stop()
		initial := <-observed.calls
		if !initial.active || initial.err != nil {
			t.Fatal("initial real renewal failed", initial)
		}
		setClock(9 * time.Minute)
		if err := r.SaveReplayResult(ownerContext, id, result); err != nil {
			t.Fatal("actual complete response was not persisted", err)
		}
		current, err := r.GetUsage(ctx, id)
		if err != nil || current.ExecutionStatus != "SETTLEMENT_PENDING" || current.Status != "REVIEW" {
			t.Fatal("missing-usage response state contract changed", current, err)
		}
		// Expiry no longer matters after this durable response. Checking expiry
		// before the receipt classification would incorrectly cancel closure.
		setClock(11 * time.Minute)
		ticks <- r.now()
		select {
		case completion := <-observed.calls:
			if completion.active || completion.err != nil {
				t.Fatal("complete receipt was classified as lost ownership", completion)
			}
		case <-ctx.Done():
			t.Fatal("real completion renewal not observed", ctx.Err())
		}
		stop()
		if cancellations.Load() != 0 {
			t.Fatal("completed response was cancelled", cancellations.Load())
		}
		select {
		case <-failed:
			t.Fatal("completed response raised lease failure")
		default:
		}
		if err := r.RecoverUncertainDispatches(ctx); err != nil {
			t.Fatal(err)
		}
		replay, err := r.LoadReplay(ctx, id, 0)
		if err != nil || replay.Result == nil || replay.Usage.ExecutionStatus != "SETTLEMENT_PENDING" {
			t.Fatal("normal loop stop lost the durable response", replay, err)
		}
	})
	t.Run("pending-label-without-complete-response-is-not-safe-stop", func(t *testing.T) {
		id, envelope := seed(t)
		defer clear(envelope.Credential)
		if err := r.MarkReview(ctx, id, "settlement_pending", map[string]any{"receipt_missing": true}); err != nil {
			t.Fatal(err)
		}
		if active, err := r.RenewDispatchLease(ctx, id, envelope.leaseOwner); active || !errors.Is(err, ErrForbidden) {
			t.Fatal("generic state label impersonated a complete response", active, err)
		}
	})
	t.Run("real-recovery-waits-for-save-and-rechecks-completed-row", func(t *testing.T) {
		id, envelope := seed(t)
		defer clear(envelope.Credential)
		config := r.pool.Config().ConnConfig.Copy()
		config.User = "token_migrator"
		owner, err := pgx.ConnectConfig(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		defer owner.Close(context.Background())
		// A fixture-only trigger pauses the real SaveReplayResult transaction
		// after its live-owner check and execution transition, before commit.
		// No runtime hook, production migration or result state is fabricated.
		if _, err := owner.Exec(ctx, `SET ROLE token_schema_owner; CREATE FUNCTION public.f05_dispatch_save_barrier() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(5052301); RETURN NEW; END $$`); err != nil {
			t.Fatal(err)
		}
		if _, err := owner.Exec(ctx, `CREATE TRIGGER f05_dispatch_save_barrier BEFORE INSERT ON public.usage_replay_results FOR EACH ROW WHEN (NEW.usage_request_id='`+id.String()+`'::uuid) EXECUTE FUNCTION public.f05_dispatch_save_barrier()`); err != nil {
			t.Fatal(err)
		}
		defer owner.Exec(context.Background(), `DROP TRIGGER IF EXISTS f05_dispatch_save_barrier ON public.usage_replay_results; DROP FUNCTION IF EXISTS public.f05_dispatch_save_barrier()`)
		if _, err := owner.Exec(ctx, `SELECT pg_advisory_lock(5052301)`); err != nil {
			t.Fatal(err)
		}
		defer owner.Exec(context.Background(), `SELECT pg_advisory_unlock(5052301)`)
		bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		setClock(9 * time.Minute)
		saved := make(chan error, 1)
		go func() { saved <- r.SaveReplayResult(withDispatchLeaseOwner(bounded, envelope.leaseOwner), id, result) }()
		f05WaitDispatchLeaseFact(t, bounded, func() bool {
			var waiting bool
			err := r.pool.QueryRow(bounded, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event='advisory' AND query LIKE 'INSERT INTO usage_replay_results%')`).Scan(&waiting)
			return err == nil && waiting
		})
		setClock(11 * time.Minute)
		recovered := make(chan error, 1)
		go func() { recovered <- r.RecoverUncertainDispatches(bounded) }()
		f05WaitDispatchLeaseFact(t, bounded, func() bool {
			var waiting bool
			err := r.pool.QueryRow(bounded, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event='transactionid' AND query LIKE 'SELECT execution_status FROM usage_requests%FOR UPDATE')`).Scan(&waiting)
			return err == nil && waiting
		})
		if _, err := owner.Exec(ctx, `SELECT pg_advisory_unlock(5052301)`); err != nil {
			t.Fatal(err)
		}
		if err := <-saved; err != nil {
			t.Fatal("actual response transaction failed", err)
		}
		if err := <-recovered; err != nil {
			t.Fatal("actual worker recovery failed", err)
		}
		replay, err := r.LoadReplay(ctx, id, 0)
		if err != nil || replay.Result == nil || replay.Usage.ExecutionStatus != "SETTLEMENT_PENDING" || replay.Usage.SettlementStatus != "USAGE_PENDING" {
			t.Fatal("stale real recovery overwrote committed response", replay, err)
		}
		var unknown int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM usage_convergence_evidence WHERE usage_request_id=$1 AND evidence_type='UNCERTAINTY' AND evidence_key='dispatch_result_unknown'`, id).Scan(&unknown); err != nil || unknown != 0 {
			t.Fatal("stale recovery published uncertainty", unknown, err)
		}
	})
}

type f05LeaseRenewalObservation struct {
	active bool
	err    error
}

// This wrapper only records the outcome of the unchanged real PostgreSQL
// method; both admission and the periodic service call execute that method.
type f05ObservedLeaseRepository struct {
	*PostgresRepository
	calls chan f05LeaseRenewalObservation
}

func (r *f05ObservedLeaseRepository) RenewDispatchLease(ctx context.Context, id uuid.UUID, owner string) (bool, error) {
	active, err := r.PostgresRepository.RenewDispatchLease(ctx, id, owner)
	r.calls <- f05LeaseRenewalObservation{active: active, err: err}
	return active, err
}

func f05WaitDispatchLeaseFact(t *testing.T, parent context.Context, ready func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for !ready() {
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("actual dispatch lease/row-lock fact was not reached", ctx.Err())
		}
	}
}
