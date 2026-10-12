package billing

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Real provider/command/persisted observation paths with controlled provider
// facts. Only the existing strict owned PostgreSQL fixture may execute this.
func TestF03ObservationCannotReopenNewerResolution(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	fixed := r.now().UTC().Truncate(time.Microsecond)
	businessDate := normalizedReconciliationBusinessDate(fixed.In(time.FixedZone("Asia/Shanghai", 8*60*60)))
	r.now = func() time.Time { return fixed }
	_, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
	actor := uuid.New()
	row := pipelineRefundRow(order, fixed)
	provider := &trackedBillProvider{reconciliationFixtureProvider: reconciliationFixtureProvider{rows: []ProviderBillEntry{row}}}
	c, err := NewPaymentCoordinatorWithProvider(r, provider, "test")
	if err != nil {
		t.Fatal(err)
	}
	c.now = r.now
	first, err := c.RunScheduledReconciliation(ctx, order.Channel, businessDate)
	if err != nil {
		t.Fatal(err)
	}
	cases, err := r.AdminReconciliationCases(ctx, first.ID, 10)
	if err != nil || len(cases) != 1 {
		t.Fatal("controlled difference", cases, err)
	}
	id := cases[0].ID
	if _, err := r.UpdateReconciliationCase(ctx, id, ReconciliationCaseCommand{Action: "CLAIM", ActorID: actor, IdempotencyKey: uuid.NewString(), Reason: "controlled case owner claim"}); err != nil {
		t.Fatal(err)
	}
	var digest string
	if err := r.pool.QueryRow(ctx, `SELECT fact_digest FROM reconciliation_cases WHERE id=$1`, id).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	lock, err := r.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	scope := "reconciliation-case-fact"
	key := fmt.Sprintf("%d:%s%s", len(scope), scope, digest)
	if _, err := lock.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1,0))`, key); err != nil {
		lock.Release()
		t.Fatal(err)
	}
	oldCtx, cancelOld := context.WithCancel(ctx)
	type oldResult struct {
		run ReconciliationRun
		err error
	}
	result := make(chan oldResult, 1)
	oldActor := uuid.New()
	released, joined := false, false
	t.Cleanup(func() {
		cancelOld()
		if !released {
			_, _ = lock.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtextextended($1,0))`, key)
			lock.Release()
		}
		if !joined {
			select {
			case <-result:
			case <-time.After(5 * time.Second):
				t.Error("owned old observation did not stop")
			}
		}
	})
	go func() {
		run, e := c.ImportProviderBill(oldCtx, oldActor, order.Channel, businessDate, []ProviderBillEntry{row})
		result <- oldResult{run, e}
	}()
	var oldRun, observation uuid.UUID
	var sourceBefore []byte
	deadline := time.Now().Add(8 * time.Second)
	for {
		err = r.pool.QueryRow(ctx, `SELECT run.id,observation.id,observation.source_bindings FROM reconciliation_runs run JOIN reconciliation_observations observation ON observation.run_id=run.id WHERE run.requested_by=$1 AND run.source_mode='FILE_IMPORT'`, oldActor).Scan(&oldRun, &observation, &sourceBefore)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("old observation was not committed before its chunk", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A has persisted its immutable snapshot, but the fact lock prevents its
	// case chunk. B now performs the genuine clean provider verification/RESOLVE.
	cleanProvider := &trackedBillProvider{}
	clean, err := NewPaymentCoordinatorWithProvider(r, cleanProvider, "test")
	if err != nil {
		t.Fatal(err)
	}
	clean.now = r.now
	command := ReconciliationCaseCommand{Action: "RESOLVE", ActorID: actor, IdempotencyKey: uuid.NewString(), Reason: "controlled current provider resolution", ReviewResult: "controlled provider difference absent"}
	if err := clean.prepareReconciliationResolution(ctx, id, &command); err != nil {
		t.Fatal(err)
	}
	resolved, err := r.UpdateReconciliationCase(ctx, id, command)
	if err != nil || resolved.Status != "RESOLVED" {
		t.Fatal("genuine resolution", resolved, err)
	}
	cancelOld()
	select {
	case old := <-result:
		joined = true
		if old.run.ID != oldRun || old.err == nil || !errors.Is(old.err, context.Canceled) {
			t.Fatal("old unfinished observation must remain recoverable", old.run, old.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("old blocked chunk did not cancel")
	}
	if _, err := lock.Exec(ctx, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, key); err != nil {
		t.Fatal(err)
	}
	lock.Release()
	released = true
	resumed, err := c.ImportProviderBill(ctx, oldActor, order.Channel, businessDate, []ProviderBillEntry{row})
	if err != nil || resumed.ID != oldRun || resumed.Status != "COMPLETED" {
		t.Fatal("resume same durable input", resumed, err)
	}
	t.Run("late_old_observation_keeps_newer_resolution", func(t *testing.T) {
		current, e := r.AdminReconciliationCases(ctx, oldRun, 10)
		if e != nil || len(current) != 1 || current[0].Status != "RESOLVED" || current[0].Version != resolved.Version {
			t.Fatal("old observation reopened a newer verified resolution", current, e)
		}
		var events int
		if e := r.pool.QueryRow(ctx, `SELECT count(*) FROM manual_review_case_events WHERE case_id=$1 AND event_type='REOPEN'`, id).Scan(&events); e != nil || events != 0 {
			t.Fatal("false reopen event", events, e)
		}
		var saved []byte
		var active uuid.UUID
		if e := r.pool.QueryRow(ctx, `SELECT source_bindings FROM reconciliation_observations WHERE id=$1`, observation).Scan(&saved); e != nil || string(saved) != string(sourceBefore) {
			t.Fatal("recovery refreshed immutable authority", e)
		}
		if e := r.pool.QueryRow(ctx, `SELECT active_observation_id FROM reconciliation_runs WHERE id=$1`, oldRun).Scan(&active); e != nil || active != observation {
			t.Fatal("recovery replaced original observation", e)
		}
	})
	t.Run("fresh_scheduled_recurrence_is_not_old_receipt", func(t *testing.T) {
		fresh, e := c.RunScheduledReconciliation(ctx, order.Channel, businessDate)
		if e != nil || fresh.ID == first.ID {
			t.Fatal("fresh provider recurrence hidden by old completed receipt", fresh, e)
		}
		current, e := r.AdminReconciliationCases(ctx, fresh.ID, 10)
		if e != nil || len(current) != 1 || current[0].ID != id || current[0].Status != "OPEN" || current[0].Version != resolved.Version+1 {
			t.Fatal("genuine recurrence did not reopen same case once", current, e)
		}
	})
	t.Run("exact_ignore_and_completed_replay_remain_accepted", func(t *testing.T) {
		if _, e := r.UpdateReconciliationCase(ctx, id, ReconciliationCaseCommand{Action: "CLAIM", ActorID: actor, IdempotencyKey: uuid.NewString(), Reason: "controlled recurrence owner claim"}); e != nil {
			t.Fatal(e)
		}
		ignored, e := r.UpdateReconciliationCase(ctx, id, ReconciliationCaseCommand{Action: "IGNORE", ActorID: actor, IdempotencyKey: uuid.NewString(), Reason: "controlled exact fact acceptance", ReviewResult: "controlled exact immutable fact accepted"})
		if e != nil {
			t.Fatal(e)
		}
		receipt, e := c.RunScheduledReconciliation(ctx, order.Channel, businessDate)
		if e != nil {
			t.Fatal(e)
		}
		replay, e := c.RunScheduledReconciliation(ctx, order.Channel, businessDate)
		if e != nil || receipt.ID != replay.ID {
			t.Fatal("unchanged accepted scheduled receipt changed", receipt, replay, e)
		}
		current, e := r.AdminReconciliationCases(ctx, replay.ID, 10)
		if e != nil || len(current) != 1 || current[0].Status != "IGNORED" || current[0].Version != ignored.Version {
			t.Fatal("exact IGNORE was reopened", current, e)
		}
	})
}
