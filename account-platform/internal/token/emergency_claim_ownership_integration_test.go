package token

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func claimOneEmergencyCancellation(t *testing.T, r *PostgresRepository, ctx context.Context, id uuid.UUID) EmergencyCancellationTarget {
	t.Helper()
	claimed, err := r.claimEmergencyCancellations(ctx, 1)
	if err != nil || len(claimed) != 1 || claimed[0].ID != id || claimed[0].ClaimAttempt <= 0 {
		t.Fatal("exact production emergency claim", claimed, err)
	}
	return claimed[0]
}

func makeEmergencyTestClaimDue(t *testing.T, r *PostgresRepository, ctx context.Context, id uuid.UUID) {
	t.Helper()
	if _, err := r.pool.Exec(ctx, `UPDATE model_emergency_cancellations SET next_attempt_at=$2 WHERE id=$1 AND status='PENDING' AND next_attempt_at IS NOT NULL`, id, r.now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestEmergencyCancellationClaimOwnershipPostgres(t *testing.T) {
	for _, stage := range []string{"before-request", "after-request", "pending", "zero-attempt"} {
		t.Run(stage, func(t *testing.T) {
			f := newModelExpiryObservationFixture(t)
			u, id := f.preparedExpiry(t, "hold")
			stale := EmergencyCancellationTarget{ID: id}
			if stage != "zero-attempt" {
				stale = claimOneEmergencyCancellation(t, f.r, f.ctx, id)
			}
			if stage == "after-request" {
				if pre, terminal, err := f.r.requestEmergencyCancellation(f.ctx, stale, ProviderCancelUnsupported, f.service.upstream.AdapterVersion()); err != nil || !pre || terminal {
					t.Fatal("original owned request", pre, terminal, err)
				}
			}
			if stage == "pending" {
				if err := f.r.retryEmergencyCancellation(f.ctx, stale, errors.New("controlled original failure")); err != nil {
					t.Fatal(err)
				}
			}
			var owner EmergencyCancellationTarget
			if stage == "before-request" || stage == "after-request" {
				// Expiry controls eligibility only; ownership comes from the real next claim.
				if _, err := f.r.pool.Exec(f.ctx, `UPDATE model_emergency_cancellations SET lease_until=$2,next_attempt_at=$2 WHERE id=$1`, id, f.r.now().Add(-time.Second)); err != nil {
					t.Fatal(err)
				}
				owner = claimOneEmergencyCancellation(t, f.r, f.ctx, id)
				if owner.ClaimAttempt != stale.ClaimAttempt+1 {
					t.Fatal("reclaim did not advance token")
				}
			}
			facts := func() string {
				t.Helper()
				var value string
				if err := f.r.pool.QueryRow(f.ctx, `SELECT jsonb_build_object('usage',to_jsonb(request),'target',to_jsonb(cancellation),'model_evidence',(SELECT coalesce(jsonb_agg(to_jsonb(e) ORDER BY e.id),'[]'::jsonb) FROM model_emergency_cancellation_evidence e WHERE e.emergency_cancellation_id=$2),'usage_evidence',(SELECT coalesce(jsonb_agg(to_jsonb(e) ORDER BY e.id),'[]'::jsonb) FROM usage_cancellation_evidence e WHERE e.usage_request_id=$1),'convergence',(SELECT coalesce(jsonb_agg(to_jsonb(e) ORDER BY e.id),'[]'::jsonb) FROM usage_convergence_evidence e WHERE e.usage_request_id=$1))::text FROM usage_requests request JOIN model_emergency_cancellations cancellation ON cancellation.usage_request_id=request.id WHERE request.id=$1 AND cancellation.id=$2`, u.ID, id).Scan(&value); err != nil {
					t.Fatal(err)
				}
				return value
			}
			fundsBefore := f05CancellationClosureFundsFacts(t, f.bp, f.ctx)
			assertStale := func() {
				t.Helper()
				before := facts()
				if _, _, err := f.r.requestEmergencyCancellation(f.ctx, stale, ProviderCancelUnsupported, f.service.upstream.AdapterVersion()); !errors.Is(err, ErrForbidden) {
					t.Fatal("stale request accepted", err)
				}
				if facts() != before {
					t.Fatal("stale request changed persisted facts")
				}
				// A stale transport result must never manufacture confirmation for the new owner.
				if err := f.r.recordEmergencyCancellationAttempt(f.ctx, stale, ProviderCancelStreamAbortConfirmed, true, false, false); !errors.Is(err, ErrForbidden) {
					t.Fatal("stale record accepted", err)
				}
				if facts() != before {
					t.Fatal("stale record changed persisted facts")
				}
				if err := f.r.retryEmergencyCancellation(f.ctx, stale, context.DeadlineExceeded); err != nil {
					t.Fatal(err)
				}
				if facts() != before {
					t.Fatal("stale retry changed persisted facts")
				}
				if f05CancellationClosureFundsFacts(t, f.bp, f.ctx) != fundsBefore {
					t.Fatal("stale operations changed financial facts")
				}
			}
			assertStale()
			if owner.ClaimAttempt == 0 {
				if stage == "pending" {
					makeEmergencyTestClaimDue(t, f.r, f.ctx, id)
				}
				owner = claimOneEmergencyCancellation(t, f.r, f.ctx, id)
			}
			pre, terminal, err := f.r.requestEmergencyCancellation(f.ctx, owner, ProviderCancelUnsupported, f.service.upstream.AdapterVersion())
			if err != nil || !pre || terminal {
				t.Fatal("new owner request", pre, terminal, err)
			}
			if _, err := f.service.finishPreDispatchCancellation(f.ctx, u.ID); err != nil {
				t.Fatal(err)
			}
			if err := f.r.recordEmergencyCancellationAttempt(f.ctx, owner, ProviderCancelUnsupported, false, pre, terminal); err != nil {
				t.Fatal("new owner record", err)
			}
			fundsBefore = f05CancellationClosureFundsFacts(t, f.bp, f.ctx)
			assertStale() // CONVERGING rejects even an obsolete matching attempt.
			stale = owner
			assertStale()
			worker := NewEmergencyModelCancellationWorker(f.r, f.service, time.Second)
			for range 2 {
				if err := worker.runOnce(f.ctx); err != nil {
					t.Fatal(err)
				}
			}
			assertStale()
			var state string
			var attempted, requested, converged int
			if err := f.r.pool.QueryRow(f.ctx, `SELECT status,(SELECT count(*) FROM model_emergency_cancellation_evidence WHERE emergency_cancellation_id=$1 AND evidence_type='REQUESTED'),(SELECT count(*) FROM model_emergency_cancellation_evidence WHERE emergency_cancellation_id=$1 AND evidence_type='ATTEMPTED'),(SELECT count(*) FROM model_emergency_cancellation_evidence WHERE emergency_cancellation_id=$1 AND evidence_type='CONVERGED') FROM model_emergency_cancellations WHERE id=$1`, id).Scan(&state, &requested, &attempted, &converged); err != nil || state != "CONVERGED" || requested != 1 || attempted != 1 || converged != 1 {
				t.Fatal("owner did not converge exactly once", state, requested, attempted, converged, err)
			}
		})
	}
}

// A request blocked on usage still owns the target row. Reclaim uses SKIP LOCKED,
// so it cannot hand a newer token out while the older request commits evidence.
func TestEmergencyCancellationRequestLocksClaimPostgres(t *testing.T) {
	t.Run("usage-lock-reclaim", func(t *testing.T) {
		f := newModelExpiryObservationFixture(t)
		u, id := f.preparedExpiry(t, "hold")
		claim := claimOneEmergencyCancellation(t, f.r, f.ctx, id)
		if _, err := f.r.pool.Exec(f.ctx, `UPDATE model_emergency_cancellations SET lease_until=$2,next_attempt_at=$2 WHERE id=$1`, id, f.r.now().Add(-time.Second)); err != nil {
			t.Fatal(err)
		}
		blocker, err := f.r.pool.Begin(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Rollback(f.ctx)
		var pid int
		if err := blocker.QueryRow(f.ctx, `SELECT pg_backend_pid() FROM usage_requests WHERE id=$1 FOR UPDATE`, u.ID).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		requestCtx, stop := context.WithTimeout(f.ctx, 10*time.Second)
		defer stop()
		go func() {
			_, _, err := f.r.requestEmergencyCancellation(requestCtx, claim, ProviderCancelUnsupported, f.service.upstream.AdapterVersion())
			done <- err
		}()
		// Observe the actual server wait instead of racing a timer with the request.
		deadline := time.Now().Add(5 * time.Second)
		for {
			var waiting bool
			if err := f.r.pool.QueryRow(f.ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("request did not reach usage lock barrier")
			}
			time.Sleep(10 * time.Millisecond)
		}
		if claimed, err := f.r.claimEmergencyCancellations(f.ctx, 1); err != nil || len(claimed) != 0 {
			t.Fatal("reclaimed request whose claim transaction is still active", claimed, err)
		}
		if err := blocker.Commit(f.ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal("owned request failed after barrier", err)
		}
		if err := f.r.retryEmergencyCancellation(f.ctx, claim, errors.New("controlled retry")); err != nil {
			t.Fatal(err)
		}
		makeEmergencyTestClaimDue(t, f.r, f.ctx, id)
		next := claimOneEmergencyCancellation(t, f.r, f.ctx, id)
		if next.ClaimAttempt != claim.ClaimAttempt+1 {
			t.Fatal("claim not reclaimable after request transaction completed")
		}
	})
}
