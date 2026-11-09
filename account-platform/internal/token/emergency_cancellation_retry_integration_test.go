package token

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestEmergencyCancellationDurableRetryPostgres(t *testing.T) {
	t.Run("late-financial-review-is-independent", func(t *testing.T) { testEmergencyCancellationReviewSeparation(t, false) })
	t.Run("settlement-does-not-close-emergency-case", func(t *testing.T) { testEmergencyCancellationReviewSeparation(t, true) })
	t.Run("unstarted-tail-does-not-spend-budget", testEmergencyCancellationClaimsOnlyWhenReady)
	for _, scenario := range []string{"failure-backoff-and-manual-case", "expired-claim-and-stale-result"} {
		t.Run(scenario, func(t *testing.T) {
			f := newModelExpiryObservationFixture(t)
			u, id := f.preparedExpiry(t, "hold")
			var laterID uuid.UUID
			if scenario == "failure-backoff-and-manual-case" {
				_, laterID = f.preparedExpiry(t, "hold")
			}
			clock := time.Now().UTC().Add(time.Second).Truncate(time.Microsecond)
			f.r.now = func() time.Time { return clock }
			// The original attempt counter is a fencing generation, not an old
			// delivery's automatic retry budget. Model a previously claimed target.
			if _, err := f.r.pool.Exec(f.ctx, `UPDATE model_emergency_cancellations SET attempts=19 WHERE id=$1`, id); err != nil {
				t.Fatal(err)
			}
			restarted := *f.r
			worker := NewEmergencyModelCancellationWorker(&restarted, f.service, time.Second)
			worker.batchSize = 1
			beforeUsage := f05CancellationRecoveryFacts(t, f.r, f.ctx, u.ID)
			beforeFunds := f05CancellationClosureFundsFacts(t, f.bp, f.ctx)
			assertNoClaim := func() {
				t.Helper()
				if claims, err := restarted.claimEmergencyCancellations(f.ctx, 1); err != nil || len(claims) != 0 {
					t.Fatal("second worker found an ineligible claim", claims, err)
				}
			}
			var last EmergencyCancellationTarget
			for n, seconds := range []int{5, 10, 20, 40, 80, 160, 300, 300} {
				last = claimOneEmergencyCancellation(t, f.r, f.ctx, id)
				if last.ClaimAttempt != 20+n {
					t.Fatal("fencing generation was reset", last)
				}
				if scenario == "expired-claim-and-stale-result" {
					leaseEnd := clock.Add(30 * time.Second)
					clock = leaseEnd.Add(time.Microsecond)
					if n == 7 {
						if err := worker.runOnce(f.ctx); err != nil {
							t.Fatal(err)
						}
					} else {
						assertNoClaim()
						clock = leaseEnd.Add(time.Duration(seconds) * time.Second)
					}
					continue
				}
				if err := f.r.retryEmergencyCancellation(f.ctx, last, errors.New("private controlled dependency failure")); err != nil {
					t.Fatal(err)
				}
				if n == 7 {
					continue
				}
				due := clock.Add(time.Duration(seconds) * time.Second)
				var scheduled time.Time
				if err := f.r.pool.QueryRow(f.ctx, `SELECT next_attempt_at FROM model_emergency_cancellations WHERE id=$1 AND status='PENDING' AND lease_until IS NULL`, id).Scan(&scheduled); err != nil || !scheduled.Equal(due) {
					t.Fatal("explicit failure schedule", scheduled, due, err)
				}
				if n == 0 {
					// A failed oldest task must not monopolize a one-slot batch.
					if err := worker.runOnce(f.ctx); err != nil {
						t.Fatal(err)
					}
					var status string
					if err := f.r.pool.QueryRow(f.ctx, `SELECT status FROM model_emergency_cancellations WHERE id=$1`, laterID).Scan(&status); err != nil || status != "CONVERGED" {
						t.Fatal("later independent hold did not converge", status, err)
					}
					beforeFunds = f05CancellationClosureFundsFacts(t, f.bp, f.ctx)
				}
				clock = due.Add(-time.Microsecond)
				assertNoClaim()
				clock = due
			}
			if err := restarted.retryEmergencyCancellation(f.ctx, last, context.DeadlineExceeded); err != nil {
				t.Fatal(err)
			}
			if err := restarted.recordEmergencyCancellationAttempt(f.ctx, last, ProviderCancelStreamAbortConfirmed, true, false, false); !errors.Is(err, ErrForbidden) {
				t.Fatal("late result overwrote the isolated target", err)
			}
			assertNoClaim()
			if got := f05CancellationRecoveryFacts(t, f.r, f.ctx, u.ID); got != beforeUsage {
				t.Fatal("retry-only bookkeeping changed usage or its proof")
			}
			if got := f05CancellationClosureFundsFacts(t, f.bp, f.ctx); got != beforeFunds {
				t.Fatal("retry exhaustion changed financial facts")
			}
			wantDiagnostic := "ATTEMPT_ERROR:DEPENDENCY_ERROR"
			if scenario == "expired-claim-and-stale-result" {
				wantDiagnostic = "ATTEMPT_ERROR:LEASE_EXPIRED"
			}
			var state, diagnostic string
			var cleared bool
			var attemptCount, evidenceCount int
			if err := f.r.pool.QueryRow(f.ctx, `SELECT status,provider_cancel_result,lease_until IS NULL AND next_attempt_at IS NULL,retry_attempts,
 (SELECT count(*) FROM model_emergency_cancellation_evidence WHERE emergency_cancellation_id=$1 AND evidence_type='MANUAL_REVIEW'
    AND snapshot=jsonb_build_object('retry_attempts',8,'diagnostic',$2::text))
 FROM model_emergency_cancellations WHERE id=$1`, id, wantDiagnostic).Scan(&state, &diagnostic, &cleared, &attemptCount, &evidenceCount); err != nil || state != "MANUAL_REVIEW" || diagnostic != wantDiagnostic || !cleared || attemptCount != 8 || evidenceCount != 1 {
				t.Fatal("durable manual record", state, diagnostic, cleared, attemptCount, evidenceCount, err)
			}
			for _, created := range []int{1, 0} {
				if count, err := restarted.SyncManualReviewCases(f.ctx, 1); err != nil || count != created {
					t.Fatal("manual case creation was lost or duplicated", count, err)
				}
			}
			var caseID uuid.UUID
			if err := f.r.pool.QueryRow(f.ctx, `SELECT id FROM manual_review_cases
 WHERE domain='TOKEN' AND case_type=$2 AND resource_type='USAGE_REQUEST' AND resource_id=$1 AND status='OPEN' AND severity='P2'`, u.ID, modelEmergencyCancellationReviewCase).Scan(&caseID); err != nil || caseID == uuid.Nil {
				t.Fatal("exhausted task is absent from the operations queue", caseID, err)
			}
		})
	}
}
