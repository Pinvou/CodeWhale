package token

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type capturedUnavailableEvidence struct{}

type capturedNumericEvidence struct {
	capturedUnavailableEvidence
	usage Usage
}

type capturedRejectedEvidence struct {
	capturedUnavailableEvidence
	entered, release chan struct{}
}

func (e capturedRejectedEvidence) Lookup(ctx context.Context, _ string) (UsageEvidence, error) {
	if e.entered != nil {
		close(e.entered)
		select {
		case <-e.release:
		case <-ctx.Done():
			return UsageEvidence{}, ctx.Err()
		}
	}
	return UsageEvidence{Rejected: true}, nil
}

func (e capturedNumericEvidence) Lookup(context.Context, string) (UsageEvidence, error) {
	return UsageEvidence{Usage: &e.usage}, nil
}

func (capturedUnavailableEvidence) ProviderID() string { return "controlled" }
func (capturedUnavailableEvidence) AdapterVersion() string {
	return NewHTTPUpstreamClient().AdapterVersion()
}
func (capturedUnavailableEvidence) Lookup(context.Context, string) (UsageEvidence, error) {
	return UsageEvidence{}, errors.New("controlled lookup outage")
}

// The real publisher/prepare transaction pauses immediately before COMMIT,
// while its usage lock is still held. No production timing hook is installed.
type capturedCommitBarrier struct {
	hit, release chan struct{}
	armed        atomic.Bool
}

func (b *capturedCommitBarrier) TraceQueryStart(ctx context.Context, _ *pgx.Conn, q pgx.TraceQueryStartData) context.Context {
	if strings.EqualFold(q.SQL, "commit") && b.armed.Swap(false) {
		close(b.hit)
		select {
		case <-b.release:
		case <-ctx.Done():
		}
	}
	return ctx
}
func (*capturedCommitBarrier) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestCapturedReferenceDeadlinePublicPostgres(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	r, ctx := retainedManagementTokenRepository(t)
	funds, _, pool := retainedManagementBillingRepository(t)
	var clock atomic.Int64
	r.now = func() time.Time {
		if n := clock.Load(); n != 0 {
			return time.Unix(0, n).UTC()
		}
		return time.Now().UTC()
	}
	var calls atomic.Int64
	var loseACK atomic.Bool
	var failCommit atomic.Bool
	handler := billing.NewService(funds).Handler("controlled-captured-money")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.URL.Path == "/internal/v1/holds/commit" && failCommit.Swap(false) {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if q.URL.Path == "/internal/v1/holds/release" {
			calls.Add(1)
			if loseACK.Swap(false) {
				out := httptest.NewRecorder()
				handler.ServeHTTP(out, q)
				if out.Code != 200 {
					t.Errorf("real release before lost ACK: %d %s", out.Code, out.Body.String())
				}
				w.WriteHeader(503)
				return
			}
		}
		handler.ServeHTTP(w, q)
	}))
	defer server.Close()
	client, err := NewHTTPBillingClient(server.URL, "controlled-captured-money")
	if err != nil {
		t.Fatal(err)
	}
	seed := func() f05DeadlineCase {
		c := f05DeadlineSeedThroughChat(t, ctx, r, funds, pool, client, &clock, time.Now().UTC().Add(-25*time.Hour), time.Now().UTC().Add(-3*time.Hour), true, false)
		if err := r.RecordUpstreamRequest(ctx, c.usage.ID, UpstreamRequestEvidence{ProviderID: "controlled", AdapterVersion: NewHTTPUpstreamClient().AdapterVersion(), RequestID: uuid.NewString()}); err != nil {
			t.Fatal(err)
		}
		return c
	}
	prepare := func(c f05DeadlineCase) *capturedReferenceRelease {
		v, handled, err := r.PrepareCapturedReferenceRelease(ctx, c.usage.ID, "USAGE_EVIDENCE_UNAVAILABLE", true)
		if err != nil || !handled || v == nil {
			t.Fatalf("prepare: %+v %v %v", v, handled, err)
		}
		return v
	}
	t.Run("trusted-lookup-survives-next-query-failure", func(t *testing.T) {
		c := seed()
		items, err := r.ClaimReconciliation(ctx, 50, 45*time.Second)
		if err != nil || len(items) != 1 || items[0].ID != c.usage.ID {
			t.Fatal("actual initial claim", len(items), err)
		}
		failCommit.Store(true)
		if err := NewUsageReconciler(r, client, capturedNumericEvidence{}, time.Second).reconcile(ctx, items[0]); err == nil {
			t.Fatal("controlled Billing Commit5xx missing")
		}
		v, _, err := r.PrepareCapturedReferenceRelease(ctx, c.usage.ID, "USAGE_EVIDENCE_UNAVAILABLE", true)
		if !errors.Is(err, ErrForbidden) || v != nil {
			t.Fatalf("lost original zero-usage evidence: %+v %v", v, err)
		}
		before := calls.Load()
		if _, err := r.pool.Exec(ctx, `UPDATE usage_requests SET reconciliation_lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, c.usage.ID); err != nil {
			t.Fatal(err)
		}
		NewUsageReconciler(r, client, capturedUnavailableEvidence{}, time.Second).runOnce(ctx)
		var holdStatus string
		if err := pool.QueryRow(ctx, `SELECT status FROM usage_holds WHERE id=$1`, c.usage.HoldID).Scan(&holdStatus); err != nil {
			t.Fatal(err)
		}
		if calls.Load() != before || holdStatus != "ACTIVE" {
			t.Fatalf("current lookup failure sent Release for known usage: RPC delta=%d real hold=%s", calls.Load()-before, holdStatus)
		}
	})
	for _, publisherFirst := range []bool{true, false} {
		name := "deadline-wins-before-numeric-publisher"
		if publisherFirst {
			name = "numeric-publisher-wins-before-deadline"
		}
		t.Run(name, func(t *testing.T) {
			c := seed()
			claim := func() ReconciliationItem {
				items, err := r.ClaimReconciliation(ctx, 50, 45*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				for _, item := range items {
					if item.ID == c.usage.ID {
						return item
					}
				}
				t.Fatal("actual ClaimReconciliation did not return original usage")
				return ReconciliationItem{}
			}
			firstClaim := claim()
			// Expire only the fixture's scheduler lease to obtain a real second
			// claim while the first process still owns its returned old snapshot.
			if _, err := r.pool.Exec(ctx, `UPDATE usage_requests SET reconciliation_lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, c.usage.ID); err != nil {
				t.Fatal(err)
			}
			secondClaim := claim()
			barrier := &capturedCommitBarrier{hit: make(chan struct{}), release: make(chan struct{})}
			barrier.armed.Store(true)
			cfg := r.pool.Config()
			cfg.ConnConfig.Tracer = barrier
			pausedPool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer pausedPool.Close()
			paused := *r
			paused.pool = pausedPool
			publish := func(repo *PostgresRepository) error {
				worker := NewUsageReconciler(repo, client, capturedNumericEvidence{usage: Usage{InputTokens: 2, OutputTokens: 3}}, time.Second)
				return worker.reconcile(ctx, firstClaim)
			}
			prepareOn := func(repo *PostgresRepository) error {
				worker := NewUsageReconciler(repo, client, capturedUnavailableEvidence{}, time.Second)
				handled, err := worker.reconcileCapturedReferenceDeadline(ctx, secondClaim, "USAGE_EVIDENCE_UNAVAILABLE", true)
				if !handled && err == nil {
					return errors.New("original deadline claim was not handled")
				}
				return err
			}
			first, second := make(chan error, 1), make(chan error, 1)
			if publisherFirst {
				go func() { first <- publish(&paused) }()
			} else {
				go func() { first <- prepareOn(&paused) }()
			}
			select {
			case <-barrier.hit:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			released := false
			defer func() {
				if !released {
					close(barrier.release)
				}
			}()
			if publisherFirst {
				go func() { second <- prepareOn(r) }()
			} else {
				go func() { second <- publish(r) }()
			}
			blocked := false
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND cardinality(pg_blocking_pids(pid))>0)`).Scan(&blocked); err != nil {
					t.Fatal(err)
				}
				if blocked {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			close(barrier.release)
			released = true
			if !blocked {
				t.Fatal("second real transaction did not wait on first usage lock")
			}
			if err := <-first; err != nil {
				t.Fatal(err)
			}
			err = <-second
			if publisherFirst {
				if !errors.Is(err, ErrForbidden) {
					t.Fatal("published numeric fact lost deadline race", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if fenced, err := r.PreserveMissingReferenceLateUsage(ctx, c.usage.ID, Usage{InputTokens: 2, OutputTokens: 3}); err != nil || !fenced {
					t.Fatal("late usage escaped decision", fenced, err)
				}
			}
			var holdStatus string
			if err := pool.QueryRow(ctx, `SELECT status FROM usage_holds WHERE id=$1`, c.usage.HoldID).Scan(&holdStatus); err != nil {
				t.Fatal(err)
			}
			want := "RELEASED"
			if publisherFirst {
				want = "COMMITTED"
			}
			if holdStatus != want {
				t.Fatalf("two real claimed reconcilers financial outcome=%s expected=%s", holdStatus, want)
			}
		})
	}
	for _, publisherFirst := range []bool{true, false} {
		name := "deadline-unknown-before-late-rejection"
		if publisherFirst {
			name = "rejection-publication-before-deadline"
		}
		t.Run(name, func(t *testing.T) {
			c := seed()
			claim := func() ReconciliationItem {
				items, err := r.ClaimReconciliation(ctx, 50, 45*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				for _, item := range items {
					if item.ID == c.usage.ID {
						return item
					}
				}
				t.Fatal("actual rejected usage claim missing")
				return ReconciliationItem{}
			}
			old := claim()
			if _, err := r.pool.Exec(ctx, `UPDATE usage_requests SET reconciliation_lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, c.usage.ID); err != nil {
				t.Fatal(err)
			}
			newer := claim()
			barrier := &capturedCommitBarrier{hit: make(chan struct{}), release: make(chan struct{})}
			barrier.armed.Store(true)
			cfg := r.pool.Config()
			cfg.ConnConfig.Tracer = barrier
			pausedPool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer pausedPool.Close()
			paused := *r
			paused.pool = pausedPool
			evidence := capturedRejectedEvidence{entered: make(chan struct{}), release: make(chan struct{})}
			publish := func(repo *PostgresRepository) error {
				return NewUsageReconciler(repo, client, evidence, time.Second).reconcile(ctx, old)
			}
			cutoff := func(repo *PostgresRepository) error {
				_, err := NewUsageReconciler(repo, client, capturedUnavailableEvidence{}, time.Second).reconcileCapturedReferenceDeadline(ctx, newer, "USAGE_EVIDENCE_UNAVAILABLE", true)
				return err
			}
			before := calls.Load()
			loseACK.Store(true)
			pub, cut := make(chan error, 1), make(chan error, 1)
			if publisherFirst {
				go func() { pub <- publish(&paused) }()
			} else {
				go func() { pub <- publish(r) }()
			}
			<-evidence.entered // The actual old claim is inside a slow Lookup.
			if publisherFirst {
				close(evidence.release)
			} else {
				go func() { cut <- cutoff(&paused) }()
			}
			select {
			case <-barrier.hit:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			released := false
			defer func() {
				if !released {
					close(barrier.release)
				}
			}()
			if publisherFirst {
				go func() { cut <- cutoff(r) }()
			} else {
				close(evidence.release)
			}
			blocked := false
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND cardinality(pg_blocking_pids(pid))>0)`).Scan(&blocked); err != nil {
					t.Fatal(err)
				}
				if blocked {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			close(barrier.release)
			released = true
			if !blocked {
				t.Fatal("rejection publication and deadline did not share usage lock")
			}
			pubErr, cutErr := <-pub, <-cut
			if publisherFirst {
				if pubErr == nil || errors.Is(pubErr, errManualReconciliationRequired) || !errors.Is(cutErr, ErrForbidden) {
					t.Fatal("rejection publication lost ownership or normal RPC error changed", pubErr, cutErr)
				}
			} else if !errors.Is(pubErr, errManualReconciliationRequired) || cutErr == nil {
				t.Fatal("late rejected claim escaped stopped unknown decision", pubErr, cutErr)
			}
			if calls.Load() != before+1 {
				t.Fatal("rejection bypassed one-send ownership", calls.Load()-before)
			}
			if publisherFirst {
				var decisions int
				if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM usage_convergence_evidence WHERE usage_request_id=$1 AND evidence_key=$2`, c.usage.ID, capturedReferenceReleaseKey).Scan(&decisions); err != nil || decisions != 0 {
					t.Fatal("rejection created missing-fact intent", decisions, err)
				}
			} else {
				NewUsageReconciler(r, client, capturedUnavailableEvidence{}, time.Second).runOnce(ctx)
				if calls.Load() != before+1 {
					t.Fatal("late rejection reopened stopped decision")
				}
				u, err := r.GetUsage(ctx, c.usage.ID)
				if err != nil {
					t.Fatal(err)
				}
				reference, actor := uuid.New(), uuid.New()
				command := runtimeGovernedCommand{ResourceID: u.ID, ExpectedVersion: u.UpdatedAt, EvidenceHighWatermark: u.LastPersistedSequence}
				requeue := func() {
					tx, err := r.pool.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback(ctx)
					if _, _, _, err := r.executeGovernedUsageReconciliationTx(ctx, tx, reference, actor, "controlled late rejection original recovery", command); err != nil {
						t.Fatal(err)
					}
					if err := tx.Commit(ctx); err != nil {
						t.Fatal(err)
					}
				}
				requeue()
				NewUsageReconciler(r, client, capturedUnavailableEvidence{}, time.Second).runOnce(ctx)
				if calls.Load() != before+2 {
					t.Fatal("fresh human command did not resume original deadline after late rejection")
				}
				requeue() // Completed original command remains only a receipt replay.
				NewUsageReconciler(r, client, capturedUnavailableEvidence{}, time.Second).runOnce(ctx)
				if calls.Load() != before+2 {
					t.Fatal("original command replay resent after late rejection")
				}
			}
		})
	}
	for _, incomplete := range []bool{false, true} {
		name := "late-complete-adapter-rejection"
		if incomplete {
			name = "late-incomplete-adapter-rejection"
		}
		t.Run(name, func(t *testing.T) {
			c := seed()
			v := prepare(c)
			loseACK.Store(true)
			if _, err := client.ReleaseCapturedReference(ctx, *v); err == nil {
				t.Fatal("lost ACK missing")
			}
			if err := r.PauseCapturedReferenceRelease(ctx, *v); err != nil {
				t.Fatal(err)
			}
			before := calls.Load()
			body := `{"error":"controlled rejection"}`
			if incomplete {
				body = strings.Repeat("x", (2<<20)+1)
			}
			response := &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
			out := httptest.NewRecorder()
			c.service.handleUpstreamFailure(out, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), c.usage.ID, *c.usage.HoldID, response)
			if calls.Load() != before || out.Code != http.StatusServiceUnavailable {
				t.Fatal("late adapter rejection sent Release", calls.Load()-before, out.Code)
			}
		})
	}
	t.Run("saved-rejection-shortcut-remains-normal", func(t *testing.T) {
		c := seed()
		if err := r.MarkReview(ctx, c.usage.ID, "upstream_body_incomplete", map[string]any{"http_status": http.StatusBadRequest, "response_disposition": responseDispositionRejectedBeforeGeneration}); err != nil {
			t.Fatal(err)
		}
		if v, _, err := r.PrepareCapturedReferenceRelease(ctx, c.usage.ID, "USAGE_EVIDENCE_UNAVAILABLE", true); !errors.Is(err, ErrForbidden) || v != nil {
			t.Fatal("saved rejection became missing fact", v, err)
		}
		before := calls.Load()
		NewUsageReconciler(r, client, capturedUnavailableEvidence{}, time.Second).runOnce(ctx)
		u, err := r.GetUsage(ctx, c.usage.ID)
		if err != nil || u.Status != "RELEASED" || calls.Load() != before+1 {
			t.Fatal("ordinary rejection shortcut changed", u.Status, calls.Load()-before, err)
		}
	})
	t.Run("saved-rejection-fact-survives-diagnostic-review", func(t *testing.T) {
		c := seed()
		if err := r.MarkReview(ctx, c.usage.ID, "upstream_body_incomplete", map[string]any{"http_status": http.StatusBadRequest, "response_disposition": responseDispositionRejectedBeforeGeneration}); err != nil {
			t.Fatal(err)
		}
		if err := r.MarkReview(ctx, c.usage.ID, "hold_renewal_failed", map[string]any{"upstream_cancelled": true}); err != nil {
			t.Fatal(err)
		}
		if v, _, err := r.PrepareCapturedReferenceRelease(ctx, c.usage.ID, "USAGE_EVIDENCE_UNAVAILABLE", true); !errors.Is(err, ErrForbidden) || v != nil {
			t.Fatal("diagnostic review erased original durable rejection", v, err)
		}
	})
	t.Run("complete-rejection-fact-survives-diagnostic-review", func(t *testing.T) {
		c := f05DeadlineSeedThroughChat(t, ctx, r, funds, pool, client, &clock, time.Now().UTC().Add(-25*time.Hour), time.Now().UTC(), false, false)
		repo := c.service.repository.(*f05DeadlineCatalogRepository)
		repo.acquire = true
		envelope, acquired, err := repo.AcquireDispatch(ctx, c.usage.ID, "controlled-rejection-owner", time.Minute)
		clock.Store(0)
		if err != nil || !acquired {
			t.Fatal("real original dispatch not acquired", acquired, err)
		}
		owned := withDispatchLeaseOwner(ctx, envelope.leaseOwner)
		if err := r.MarkUpstream(owned, c.usage.ID); err != nil {
			t.Fatal(err)
		}
		if err := r.RecordUpstreamRequest(owned, c.usage.ID, UpstreamRequestEvidence{ProviderID: "controlled", AdapterVersion: NewHTTPUpstreamClient().AdapterVersion(), RequestID: uuid.NewString()}); err != nil {
			t.Fatal(err)
		}
		before := calls.Load()
		loseACK.Store(true)
		out := httptest.NewRecorder()
		c.service.handleUpstreamFailure(out, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(owned), c.usage.ID, *c.usage.HoldID, &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":"controlled rejection"}`))})
		if out.Code != http.StatusBadRequest || calls.Load() != before+1 {
			t.Fatal("normal complete rejection or original RPC result changed", out.Code, calls.Load()-before)
		}
		if err := r.MarkReview(ctx, c.usage.ID, "hold_renewal_failed", map[string]any{"upstream_cancelled": true}); err != nil {
			t.Fatal(err)
		}
		if v, _, err := r.PrepareCapturedReferenceRelease(ctx, c.usage.ID, "USAGE_EVIDENCE_UNAVAILABLE", true); !errors.Is(err, ErrForbidden) || v != nil {
			t.Fatal("diagnostic review erased complete durable rejection", v, err)
		}
	})
	t.Run("lost-ack-needs-new-human-command-original-intent", func(t *testing.T) {
		c := seed()
		v := prepare(c)
		loseACK.Store(true)
		if _, err := client.ReleaseCapturedReference(ctx, *v); err == nil {
			t.Fatal("lost ACK accepted")
		}
		if err := r.PauseCapturedReferenceRelease(ctx, *v); err != nil {
			t.Fatal(err)
		}
		before := calls.Load()
		NewUsageReconciler(r, client, capturedUnavailableEvidence{}, time.Second).runOnce(ctx)
		NewUsageReconciler(r, client, capturedUnavailableEvidence{}, time.Second).runOnce(ctx)
		for i := 0; i < 2; i++ {
			again, handled, err := r.PrepareCapturedReferenceRelease(ctx, c.usage.ID, "", false)
			if err != nil || !handled || again != nil {
				t.Fatalf("old claim received RPC right: %+v %v %v", again, handled, err)
			}
		}
		var losses int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM usage_platform_loss_reviews WHERE usage_request_id=$1`, c.usage.ID).Scan(&losses); err != nil || losses != 0 {
			t.Fatal("unconfirmed release wrote loss", losses, err)
		}
		ref, actor := uuid.New(), uuid.New()
		commands := map[uuid.UUID]runtimeGovernedCommand{}
		requeue := func(reference uuid.UUID) {
			u, err := r.GetUsage(ctx, c.usage.ID)
			if err != nil {
				t.Fatal(err)
			}
			tx, err := r.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			command, found := commands[reference]
			if !found {
				command = runtimeGovernedCommand{ResourceID: u.ID, ExpectedVersion: u.UpdatedAt, EvidenceHighWatermark: u.LastPersistedSequence}
				commands[reference] = command
			}
			_, _, _, err = r.executeGovernedUsageReconciliationTx(ctx, tx, reference, actor, "controlled original intent recovery", command)
			if err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
		}
		requeue(ref)
		restored, handled, err := r.PrepareCapturedReferenceRelease(ctx, c.usage.ID, "", false)
		if err != nil || !handled || restored == nil {
			t.Fatalf("human original intent unavailable: %+v %v", restored, err)
		}
		if !expiryJSONEqual(mustCapturedJSON(restored.snapshot()), mustCapturedJSON(v.snapshot())) {
			t.Fatal("human retry changed decision")
		}
		// The old governed command is a read-only replay, not a second requeue.
		requeue(ref)
		if duplicate, _, err := r.PrepareCapturedReferenceRelease(ctx, c.usage.ID, "", false); err != nil || duplicate != nil {
			t.Fatal("completed command reopened sending", err)
		}
		loseACK.Store(true)
		if _, err := client.ReleaseCapturedReference(ctx, *restored); err == nil {
			t.Fatal("manual attempt's lost ACK missing")
		}
		if err := r.PauseCapturedReferenceRelease(ctx, *restored); err != nil {
			t.Fatal(err)
		}
		requeue(ref)
		if duplicate, _, err := r.PrepareCapturedReferenceRelease(ctx, c.usage.ID, "", false); err != nil || duplicate != nil {
			t.Fatal("old command requeue repeated after failure", err)
		}
		requeue(uuid.New())
		restored, _, err = r.PrepareCapturedReferenceRelease(ctx, c.usage.ID, "", false)
		if err != nil || restored == nil {
			t.Fatal("new human command did not recover original intent", err)
		}
		receipt, err := client.ReleaseCapturedReference(ctx, *restored)
		if err != nil {
			t.Fatal(err)
		}
		if err = r.FinishCapturedReferenceRelease(ctx, *restored, receipt); err != nil {
			t.Fatal(err)
		}
		if err = r.FinishCapturedReferenceRelease(ctx, *restored, receipt); err != nil {
			t.Fatal(err)
		}
		if calls.Load() != before+2 {
			t.Fatal("extra release RPC")
		}
		u, err := r.GetUsage(ctx, c.usage.ID)
		if err != nil || u.Status != "REVIEW" || u.SettlementStatus != "REVIEW" {
			t.Fatalf("generic outcome changed: %+v %v", u, err)
		}
	})
	t.Run("prepare-before-rpc-crash-does-not-resend", func(t *testing.T) {
		c := seed()
		_ = prepare(c)
		if v, handled, err := r.PrepareCapturedReferenceRelease(ctx, c.usage.ID, "", false); err != nil || !handled || v != nil {
			t.Fatalf("crash auto-recovery: %+v %v", v, err)
		}
	})
	for _, waiverFirst := range []bool{true, false} {
		name := "captured-before-waiver"
		if waiverFirst {
			name = "waiver-before-captured"
		}
		t.Run(name, func(t *testing.T) {
			c := seed()
			command, err := funds.PrepareUsageWaiver(ctx, *c.usage.HoldID, "controlled original waiver", uuid.NewString(), billing.UsageWaiverActor{ID: uuid.New(), Issuer: "controlled", Subject: "controlled", AuthorizationVersion: 1})
			if err != nil {
				t.Fatal(err)
			}
			if !waiverFirst {
				_ = prepare(c)
			}
			decision, err := r.PrepareUsageWaiver(ctx, command)
			if err != nil {
				t.Fatal(err)
			}
			if waiverFirst {
				if decision.Admission != "ADMITTED" {
					t.Fatal(decision)
				}
				if v, _, err := r.PrepareCapturedReferenceRelease(ctx, c.usage.ID, "USAGE_EVIDENCE_UNAVAILABLE", true); !errors.Is(err, ErrForbidden) || v != nil {
					t.Fatal("captured replaced original waiver", err)
				}
			} else if decision.Admission != "REJECTED" || decision.ReasonCode != "DEADLINE_RELEASE_OWNS_RECOVERY" {
				t.Fatal("new waiver replaced original captured decision", decision)
			}
		})
	}
	t.Run("finisher-commit-abort-keeps-manual-original-intent", func(t *testing.T) {
		c := seed()
		v := prepare(c)
		receipt, err := client.ReleaseCapturedReference(ctx, *v)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := pgx.ParseConfig(os.Getenv("PINVOU_TEST_TOKEN_DATABASE_URL"))
		if err != nil {
			t.Fatal(err)
		}
		cfg.Database = r.pool.Config().ConnConfig.Database
		if !strings.HasPrefix(cfg.Database, "f04_token_management_") {
			t.Fatal("fault DDL requires owned fixture database")
		}
		owner, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer owner.Close(context.Background())
		if _, err = owner.Exec(ctx, `CREATE FUNCTION public.captured_finish_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.reconciliation_last_error LIKE 'TRUSTED_USAGE_DEADLINE_EXCEEDED:%' THEN RAISE EXCEPTION 'controlled finisher commit abort'; END IF; RETURN NEW; END $$; CREATE CONSTRAINT TRIGGER captured_finish_abort AFTER UPDATE ON public.usage_requests DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.captured_finish_abort()`); err != nil {
			t.Fatal(err)
		}
		defer owner.Exec(context.Background(), `DROP TRIGGER captured_finish_abort ON public.usage_requests; DROP FUNCTION public.captured_finish_abort()`)
		if err = r.FinishCapturedReferenceRelease(ctx, *v, receipt); err == nil {
			t.Fatal("controlled COMMIT abort did not fail")
		}
		var losses, confirmed int
		var stopped bool
		if err = r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM usage_platform_loss_reviews WHERE usage_request_id=$1),(SELECT count(*) FROM usage_convergence_evidence WHERE usage_request_id=$1 AND evidence_type='CUSTOMER_HOLD_RELEASE' AND source_type='BILLING'),reconciliation_stopped_at IS NOT NULL FROM usage_requests WHERE id=$1`, c.usage.ID).Scan(&losses, &confirmed, &stopped); err != nil || losses != 0 || confirmed != 0 || !stopped {
			t.Fatal("aborted confirmation changed original stop or money attribution", losses, confirmed, stopped, err)
		}
		before := calls.Load()
		NewUsageReconciler(r, client, capturedUnavailableEvidence{}, time.Second).runOnce(ctx)
		if calls.Load() != before {
			t.Fatal("finisher failure silently auto-retried Release")
		}
	})
	t.Run("legacy-metadata-is-not-trusted-numeric-usage", func(t *testing.T) {
		c := seed()
		// Historical shape from 000014/000015, not a current canonical0 producer.
		// Funding/hold/reference remain the real original writers' facts.
		if _, err := r.pool.Exec(ctx, `UPDATE usage_requests SET canonicalization_version=0,request_fingerprint='',request_snapshot='{}',usage_snapshot='{"diagnostic":"no-numeric-usage"}',trusted_usage_snapshot='{"diagnostic":"no-numeric-usage"}',trusted_usage_source=NULL WHERE id=$1`, c.usage.ID); err != nil {
			t.Fatal(err)
		}
		v := prepare(c)
		receipt, err := client.ReleaseCapturedReference(ctx, *v)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.FinishCapturedReferenceRelease(ctx, *v, receipt); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("missing-reference-intent-keeps-original-owner", func(t *testing.T) {
		c := f05DeadlineSeedThroughChat(t, ctx, r, funds, pool, client, &clock, time.Now().UTC().Add(-25*time.Hour), time.Now().UTC().Add(-3*time.Hour), true, false)
		request, handled, err := r.PrepareMissingReferenceRelease(ctx, c.usage.ID)
		if err != nil || !handled || request == nil {
			t.Fatal("original missing-reference intent missing", err)
		}
		if v, _, err := r.PrepareCapturedReferenceRelease(ctx, c.usage.ID, "USAGE_EVIDENCE_UNAVAILABLE", true); !errors.Is(err, ErrForbidden) || v != nil {
			t.Fatal("captured replaced original missing-reference decision", err)
		}
	})
	t.Run("captured-intent-never-becomes-missing-reference-recovery", func(t *testing.T) {
		c := seed()
		_ = prepare(c)
		if request, handled, err := r.PrepareMissingReferenceRelease(ctx, c.usage.ID); err != nil || handled || request != nil {
			t.Fatal("captured entered automatic missing-reference retry", handled, err)
		}
		if err := r.RecoverStoppedMissingReferences(ctx, 50); err != nil {
			t.Fatal(err)
		}
		if v, _, err := r.PrepareCapturedReferenceRelease(ctx, c.usage.ID, "", false); err != nil || v != nil {
			t.Fatal("missing scanner reopened captured latch", err)
		}
	})
	for _, source := range []string{"PROVIDER_FINAL_USAGE", "MIGRATED_SETTLEMENT"} {
		t.Run("known-"+source, func(t *testing.T) {
			c := seed()
			if _, err := r.pool.Exec(ctx, `UPDATE usage_requests SET trusted_usage_source=$2,trusted_usage_snapshot='{"input_tokens":0,"output_tokens":0,"cached_input_tokens":0}',final_usage_observed_at=now() WHERE id=$1`, c.usage.ID, source); err != nil {
				t.Fatal(err)
			}
			if v, _, err := r.PrepareCapturedReferenceRelease(ctx, c.usage.ID, "USAGE_EVIDENCE_UNAVAILABLE", true); !errors.Is(err, ErrForbidden) || v != nil {
				t.Fatal("known final usage was released", err)
			}
		})
	}
	t.Run("prepare-commit-abort-never-grants-rpc", func(t *testing.T) {
		c := seed()
		cfg, err := pgx.ParseConfig(os.Getenv("PINVOU_TEST_TOKEN_DATABASE_URL"))
		if err != nil {
			t.Fatal(err)
		}
		cfg.Database = r.pool.Config().ConnConfig.Database
		if !strings.HasPrefix(cfg.Database, "f04_token_management_") {
			t.Fatal("fault DDL requires owned fixture database")
		}
		owner, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer owner.Close(context.Background())
		if _, err = owner.Exec(ctx, `CREATE FUNCTION public.captured_prepare_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.reconciliation_last_error='CAPTURED_REFERENCE_RELEASE_REQUIRES_MANUAL_RECOVERY' THEN RAISE EXCEPTION 'controlled prepare commit abort'; END IF; RETURN NEW; END $$; CREATE CONSTRAINT TRIGGER captured_prepare_abort AFTER UPDATE ON public.usage_requests DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.captured_prepare_abort()`); err != nil {
			t.Fatal(err)
		}
		defer owner.Exec(context.Background(), `DROP TRIGGER captured_prepare_abort ON public.usage_requests; DROP FUNCTION public.captured_prepare_abort()`)
		v, _, err := r.PrepareCapturedReferenceRelease(ctx, c.usage.ID, "USAGE_EVIDENCE_UNAVAILABLE", true)
		if err == nil || v != nil {
			t.Fatal("uncommitted prepare granted send right", v, err)
		}
		var intents int
		var stopped bool
		if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM usage_convergence_evidence WHERE usage_request_id=$1 AND evidence_key=$2),reconciliation_stopped_at IS NOT NULL FROM usage_requests WHERE id=$1`, c.usage.ID, capturedReferenceReleaseKey).Scan(&intents, &stopped); err != nil || intents != 0 || stopped {
			t.Fatal("aborted prepare left partial decision", intents, stopped, err)
		}
	})
}

func TestCapturedReferenceOrganizationHoldPublicPostgres(t *testing.T) {
	f := newOrganizationCancellationFixture(t, false)
	repo := f.service.repository.(*f05DeadlineCatalogRepository)
	started := time.Now().UTC().Add(-25 * time.Hour)
	repo.acquiredAt = time.Now().UTC().Add(-3 * time.Hour)
	repo.clock.Store(started.UnixNano())
	f.repository.now = func() time.Time {
		if n := repo.clock.Load(); n != 0 {
			return time.Unix(0, n).UTC()
		}
		return time.Now().UTC()
	}
	response := f.request(http.MethodPost, "/v1/chat/completions", f.key, f.body)
	repo.clock.Store(0)
	if response.Code != http.StatusOK {
		t.Fatalf("actual organization CHAT: %d %s", response.Code, response.Body.String())
	}
	u := f.original(t)
	if err := f.repository.RecordUpstreamRequest(f.ctx, u.ID, UpstreamRequestEvidence{ProviderID: "controlled", AdapterVersion: NewHTTPUpstreamClient().AdapterVersion(), RequestID: uuid.NewString()}); err != nil {
		t.Fatal(err)
	}
	v, handled, err := f.repository.PrepareCapturedReferenceRelease(f.ctx, u.ID, "USAGE_EVIDENCE_UNAVAILABLE", true)
	if err != nil || !handled || v == nil || v.OrganizationID == nil || *v.OrganizationID != f.organization {
		t.Fatalf("original org intent: %+v %v", v, err)
	}
	client := f.service.cancellationBilling.(*HTTPBillingClient)
	receipt, err := client.ReleaseCapturedReference(f.ctx, *v)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.repository.FinishCapturedReferenceRelease(f.ctx, *v, receipt); err != nil {
		t.Fatal(err)
	}
	f.assertMoney(t, u, "RELEASED", 0)
	u, err = f.repository.GetUsage(f.ctx, u.ID)
	if err != nil || u.Status != "REVIEW" || u.SettlementStatus != "REVIEW" {
		t.Fatal("org deadline changed generic outcome", err)
	}
}

type maxProbeUpstream struct{ UpstreamClient }

func (u maxProbeUpstream) Do(ctx context.Context, url string, secret, body []byte, id uuid.UUID) (*http.Response, error) {
	response, err := u.UpstreamClient.Do(ctx, url, secret, body, id)
	if err != nil {
		return nil, err
	}
	response.Body.Close()
	response.Body = io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"controlled"}}],"usage":{"prompt_tokens":200,"completion_tokens":300}}`))
	response.Header.Set("X-Controlled-Reference", "controlled-max-probe-reference")
	return response, nil
}
func (maxProbeUpstream) UpstreamRequestID(r *http.Response) string {
	return r.Header.Get("X-Controlled-Reference")
}

type maxProbeCrash struct {
	*f05DeadlineTariffClient
	usageID        uuid.UUID
	holdID         uuid.UUID
	extensionError error
}

func (c *maxProbeCrash) Extend(ctx context.Context, usageID, holdID uuid.UUID, amount int64, key uuid.UUID) error {
	c.usageID, c.holdID = usageID, holdID
	c.extensionError = c.HTTPBillingClient.Extend(ctx, usageID, holdID, amount, key)
	panic("controlled process boundary after Extend lost ACK before final SaveReplayResult")
}

func TestCapturedReferenceMaximumACKLossPostgres(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	r, ctx := retainedManagementTokenRepository(t)
	funds, _, pool := retainedManagementBillingRepository(t)
	var clock atomic.Int64
	r.now = func() time.Time {
		if n := clock.Load(); n != 0 {
			return time.Unix(0, n).UTC()
		}
		return time.Now().UTC()
	}
	handler := billing.NewService(funds).Handler("controlled-max-probe")
	var extensionCommitted atomic.Bool
	var proofUnavailable atomic.Bool
	proofUnavailable.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.URL.Path == "/internal/v1/holds/extend" && !extensionCommitted.Load() {
			out := httptest.NewRecorder()
			handler.ServeHTTP(out, q)
			if out.Code != 200 {
				t.Errorf("real Extend failed before ACK loss: %d %s", out.Code, out.Body.String())
			}
			extensionCommitted.Store(out.Code == 200)
			w.WriteHeader(503)
			return
		}
		if q.URL.Path == "/internal/v1/holds/extend" {
			before := f05DeadlineBillingFacts(t, ctx, pool)
			if proofUnavailable.Swap(false) {
				w.WriteHeader(503)
			} else {
				handler.ServeHTTP(w, q)
			}
			f05DeadlineNoBillingChange(t, ctx, pool, before)
			return
		}
		handler.ServeHTTP(w, q)
	}))
	defer server.Close()
	client, err := NewHTTPBillingClient(server.URL, "controlled-max-probe")
	if err != nil {
		t.Fatal(err)
	}
	var crash *maxProbeCrash
	func() {
		defer func() {
			if x := recover(); x == nil {
				t.Error("crash boundary not reached")
			} else {
				t.Log(x)
			}
		}()
		f05DeadlineSeedThroughChat(t, ctx, r, funds, pool, client, &clock, time.Now().UTC().Add(-25*time.Hour), time.Now().UTC().Add(-3*time.Hour), true, true, func(s *Service) {
			crash = &maxProbeCrash{f05DeadlineTariffClient: s.executionBilling.(*f05DeadlineTariffClient)}
			s.executionBilling = crash
			s.executionUpstream = maxProbeUpstream{s.executionUpstream}
		})
	}()
	if crash == nil || crash.usageID == uuid.Nil || !extensionCommitted.Load() || crash.extensionError == nil {
		t.Fatal("actual extension/lostACK prerequisites missing", crash)
	}
	u, err := r.GetUsage(ctx, crash.usageID)
	if err != nil {
		t.Fatal(err)
	}
	var holdMax int64
	var holdStatus string
	if err = pool.QueryRow(ctx, `SELECT max_microcredits,status FROM usage_holds WHERE id=$1`, crash.holdID).Scan(&holdMax, &holdStatus); err != nil {
		t.Fatal(err)
	}
	var replayRows int
	var source *string
	if err = r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM usage_replay_results WHERE usage_request_id=$1),trusted_usage_source FROM usage_requests WHERE id=$1`, u.ID).Scan(&replayRows, &source); err != nil {
		t.Fatal(err)
	}
	t.Logf("AFTER_REAL_CHAT_EXTEND_ACK_LOSS billingMaximum=%d tokenMaximum=%d hold=%s token=%s/%s actual=%v trustedSource=%v replayRows=%d", holdMax, u.MaximumMicrocredits, holdStatus, u.Status, u.ExecutionStatus, u.ActualMicrocredits, source, replayRows)
	if holdMax <= u.MaximumMicrocredits || u.ActualMicrocredits != nil || source != nil || replayRows != 0 {
		t.Fatal("crash prefix did not produce target state")
	}
	// Fixture used the original historical dispatch clock; the genuine recovery
	// scans its already-expired worker lease. No financial/state tuple is patched.
	if err = r.RecoverUncertainDispatches(ctx); err != nil {
		t.Fatal(err)
	}
	u, err = r.GetUsage(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("AFTER_ACTUAL_LEASE_RECOVERY token=%s/%s", u.Status, u.ExecutionStatus)
	if u.Status != "REVIEW" {
		t.Fatal("real recovery did not admit original generic path")
	}
	items, err := r.ClaimReconciliation(ctx, 50, 45*time.Second)
	if err != nil || len(items) != 1 || items[0].ID != u.ID {
		t.Fatal("actual claim missing", len(items), err)
	}
	v, handled, err := r.PrepareCapturedReferenceRelease(ctx, u.ID, "USAGE_EVIDENCE_UNAVAILABLE", true)
	if err != nil || !handled || v == nil {
		t.Fatal("real captured Prepare blocked", v, handled, err)
	}
	receipt, err := client.ReleaseCapturedReference(ctx, *v)
	if err == nil {
		t.Fatal("proof503 accepted", receipt)
	}
	if err = r.PauseCapturedReferenceRelease(ctx, *v); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT max_microcredits,status FROM usage_holds WHERE id=$1`, crash.holdID).Scan(&holdMax, &holdStatus); err != nil {
		t.Fatal(err)
	}
	t.Logf("AFTER_REAL_RELEASE billing=%s max=%d frozenIntentMaximum=%d proofUnavailable=true", holdStatus, holdMax, v.Maximum)
	if holdStatus != "RELEASED" {
		t.Fatal("real Release did not commit")
	}
	u, err = r.GetUsage(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = r.executeGovernedUsageReconciliationTx(ctx, tx, uuid.New(), uuid.New(), "controlled original max intent manual recovery", runtimeGovernedCommand{ResourceID: u.ID, ExpectedVersion: u.UpdatedAt, EvidenceHighWatermark: u.LastPersistedSequence})
	if err != nil {
		tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	again, handled, err := r.PrepareCapturedReferenceRelease(ctx, u.ID, "", false)
	if err != nil || !handled || again == nil || again.Maximum != v.Maximum {
		t.Fatal("human original intent unavailable", again, err)
	}
	confirmedReceipt, err := client.ReleaseCapturedReference(ctx, *again)
	if err != nil || confirmedReceipt.Extension == nil || confirmedReceipt.Maximum != 500 || again.Maximum != 12 {
		t.Fatal("original exact proof did not converge", confirmedReceipt, err)
	}
	for _, mutate := range []string{"unknown-operation", "wrong-amount", "wrong-lease"} {
		before := f05DeadlineBillingFacts(t, ctx, pool)
		body := map[string]any{"usage_request_id": u.ID, "hold_id": crash.holdID, "operation_id": finalUsageReservationOperationID(u.ID, crash.holdID), "lease_seconds": 900, "additional_microcredits": 488}
		if mutate == "unknown-operation" {
			body["operation_id"] = uuid.New()
		}
		if mutate == "wrong-amount" {
			body["additional_microcredits"] = 489
		}
		if mutate == "wrong-lease" {
			body["lease_seconds"] = 901
		}
		if _, err := client.capturedPost200(ctx, "/internal/v1/holds/extend", body); err == nil {
			t.Fatal("terminal accepted wrong proof", mutate)
		}
		f05DeadlineNoBillingChange(t, ctx, pool, before)
	}
	if err = r.FinishCapturedReferenceRelease(ctx, *again, confirmedReceipt); err != nil {
		t.Fatal(err)
	}
	if err = r.FinishCapturedReferenceRelease(ctx, *again, confirmedReceipt); err != nil {
		t.Fatal("finisher replay", err)
	}
	var losses, confirmed int
	if err = r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM usage_platform_loss_reviews WHERE usage_request_id=$1),(SELECT count(*) FROM usage_convergence_evidence WHERE usage_request_id=$1 AND evidence_type='CUSTOMER_HOLD_RELEASE' AND source_type='BILLING')`, u.ID).Scan(&losses, &confirmed); err != nil || losses != 1 || confirmed != 1 {
		t.Fatal("wrong completion count", losses, confirmed, err)
	}
	current, err := r.GetUsage(ctx, u.ID)
	if err != nil || current.MaximumMicrocredits != 12 || current.Status != "REVIEW" || current.SettlementStatus != "REVIEW" {
		t.Fatal("intent identity or outcome changed", current, err)
	}
	t.Log("real CHAT max12/500 lostACK crash converged through exact terminal old-operation receipt; Billing proof snapshots unchanged")
}
