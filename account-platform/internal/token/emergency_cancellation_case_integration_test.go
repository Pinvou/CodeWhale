package token

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/manualreview"
	"github.com/google/uuid"
)

func testEmergencyCancellationReviewSeparation(t *testing.T, settle bool) {
	f := newModelExpiryObservationFixture(t)
	u, targetID := f.preparedExpiry(t, "hold")
	clock := time.Now().UTC().Add(time.Second).Truncate(time.Microsecond)
	f.r.now = func() time.Time { return clock }
	cause := errors.New("controlled unconfirmed hold acknowledgement")
	for n := range 8 {
		claim := claimOneEmergencyCancellation(t, f.r, f.ctx, targetID)
		if err := f.r.retryEmergencyCancellation(f.ctx, claim, cause); err != nil {
			t.Fatal(err)
		}
		if n != 7 {
			if err := f.r.pool.QueryRow(f.ctx, `SELECT next_attempt_at FROM model_emergency_cancellations WHERE id=$1`, targetID).Scan(&clock); err != nil {
				t.Fatal(err)
			}
		}
	}
	if count, err := f.r.SyncManualReviewCases(f.ctx, 2); err != nil || count != 1 {
		t.Fatal("exhaustion did not create its own review", count, err)
	}
	var cancellationID uuid.UUID
	if err := f.r.pool.QueryRow(f.ctx, `SELECT id FROM manual_review_cases WHERE resource_id=$1 AND case_type=$2 AND severity='P2' AND status='OPEN'`, u.ID, modelEmergencyCancellationReviewCase).Scan(&cancellationID); err != nil {
		t.Fatal("missing open cancellation review", err)
	}
	ids := []string{cancellationID.String()}
	if settle {
		if _, pre, err := f.r.RequestCancellation(f.ctx, u.ID, u.UserID, strings.Repeat("c", 64), cancellationFingerprint(u.ID), f.service.upstream.AdapterVersion()); err != nil || !pre {
			t.Fatal("real USER cancellation was not accepted", pre, err)
		}
		if _, err := f.service.finishPreDispatchCancellation(f.ctx, u.ID); err != nil {
			t.Fatal("existing Billing release did not confirm", err)
		}
		current, err := f.r.GetUsage(f.ctx, u.ID)
		if err != nil || current.SettlementStatus != "RELEASED" || current.CancelStatus != "LOCAL_CONFIRMED" {
			t.Fatal("expected independent release proof", current.SettlementStatus, current.CancelStatus, err)
		}
	} else {
		// The actual late uncertainty writer is the same one called when a
		// previously started Billing Release fails after its model lease expires.
		if _, err := f.service.unconfirmedCancellation(f.ctx, u, cause); !errors.Is(err, cause) {
			t.Fatal("late uncertainty did not reach the production writer", err)
		}
		if count, err := f.r.SyncManualReviewCases(f.ctx, 2); err != nil || count != 1 {
			t.Fatal("earlier cancellation review masked the financial P1", count, err)
		}
		var financialID uuid.UUID
		var claimDue time.Time
		deadlines, err := manualreview.DeadlinesFor(u.StartedAt, "P1")
		if err != nil {
			t.Fatal(err)
		}
		if err := f.r.pool.QueryRow(f.ctx, `SELECT id,claim_due_at FROM manual_review_cases WHERE resource_id=$1 AND case_type='USAGE_RECONCILIATION_REVIEW' AND severity='P1' AND status='OPEN'`, u.ID).Scan(&financialID, &claimDue); err != nil || financialID == cancellationID || !claimDue.Equal(deadlines.Claim) {
			t.Fatal("original financial review severity and SLA must be retained", claimDue, err)
		}
		ids = append(ids, financialID.String())
	}
	if count, err := f.r.SyncManualReviewCases(f.ctx, 2); err != nil || count != 0 {
		t.Fatal("repeated scan recreated a case", count, err)
	}
	items, err := f.r.adminReconciliationByIDs(f.ctx, ids)
	if err != nil || len(items) != len(ids) || items[0].ResourceID != u.ID || items[0].CaseType != modelEmergencyCancellationReviewCase || items[0].Status != "OPEN" {
		t.Fatal("operations projection dropped or closed the cancellation review", items, err)
	}
}

type emergencyTestReleaseGate struct {
	cancellationBillingClient
	first          uuid.UUID
	entered, allow chan struct{}
}

func (g emergencyTestReleaseGate) Release(ctx context.Context, usageID, holdID uuid.UUID, reason string) error {
	if usageID == g.first {
		close(g.entered)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-g.allow:
		}
	}
	return g.cancellationBillingClient.Release(ctx, usageID, holdID, reason)
}

func testEmergencyCancellationClaimsOnlyWhenReady(t *testing.T) {
	f := newModelExpiryObservationFixture(t)
	first, firstID := f.preparedExpiry(t, "hold")
	_, tailID := f.preparedExpiry(t, "hold")
	now := time.Now().UTC().Add(time.Second)
	f.r.now = func() time.Time { return now }
	gate := emergencyTestReleaseGate{f.client, first.ID, make(chan struct{}), make(chan struct{})}
	f.service.cancellationBilling = gate
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	firstWorker := NewEmergencyModelCancellationWorker(f.r, f.service, time.Second)
	firstWorker.batchSize = 2
	finished := make(chan struct{})
	var firstErr error
	go func() {
		defer close(finished)
		firstErr = firstWorker.runOnce(ctx)
	}()
	defer func() { cancel(); <-finished }()
	select {
	case <-ctx.Done():
		t.Fatal("first worker never started its actual release")
	case <-gate.entered:
	}
	var state string
	var attempts int
	if err := f.r.pool.QueryRow(f.ctx, `SELECT status,retry_attempts FROM model_emergency_cancellations WHERE id=$1`, tailID).Scan(&state, &attempts); err != nil || state != "PENDING" || attempts != 0 {
		t.Fatal("unstarted batch tail consumed its lease or budget", state, attempts, err)
	}
	restarted := *f.r
	secondWorker := NewEmergencyModelCancellationWorker(&restarted, f.service, time.Second)
	if err := secondWorker.runOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.r.pool.QueryRow(f.ctx, `SELECT status,retry_attempts FROM model_emergency_cancellations WHERE id=$1`, tailID).Scan(&state, &attempts); err != nil || state != "CONVERGED" || attempts != 1 {
		t.Fatal("other worker could not process the unstarted tail", state, attempts, err)
	}
	close(gate.allow)
	<-finished
	if firstErr != nil {
		t.Fatal(firstErr)
	}
	if err := f.r.pool.QueryRow(f.ctx, `SELECT status,retry_attempts FROM model_emergency_cancellations WHERE id=$1`, firstID).Scan(&state, &attempts); err != nil || state != "CONVERGED" || attempts != 1 {
		t.Fatal("blocked owner did not complete exactly once", state, attempts, err)
	}
}
