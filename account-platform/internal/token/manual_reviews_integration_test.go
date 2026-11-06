package token

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/manualreview"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestManualReviewSLAEscalationPostgres(t *testing.T) {
	fixture := func(t *testing.T) (financialRouteFixture, uuid.UUID, manualreview.OnCallSchedule, []uuid.UUID, func(int, int)) {
		t.Helper()
		f := financialRouteSetup(t)
		usage := f.review(t)
		var id uuid.UUID
		var usageBefore string
		var routeBefore int64
		if err := f.r.pool.QueryRow(f.ctx, `SELECT c.id,row_to_json(u)::text,r.health_version
 FROM manual_review_cases c JOIN usage_requests u ON u.id=c.resource_id
 JOIN routing_policies r ON r.id=$2 WHERE u.id=$1`, usage.ID, f.route).Scan(&id, &usageBefore, &routeBefore); err != nil {
			t.Fatal(err)
		}
		owners := []uuid.UUID{uuid.New(), uuid.New()}
		schedule, err := manualreview.ParseOnCallSchedule(owners[0].String()+","+owners[1].String(), true)
		if err != nil {
			t.Fatal(err)
		}
		check := func(wantLevel, wantStopLoss int) {
			t.Helper()
			var level, revision, escalated, evaluations, stopLossEvents, stopLossOutbox int
			var assigned []uuid.UUID
			var versions []int
			var usageAfter string
			var healthVersion int64
			err := f.r.pool.QueryRow(f.ctx, `SELECT escalation_level,assignment_revision,
 (SELECT count(*) FROM manual_review_case_events WHERE case_id=$1 AND event_type='SLA_ESCALATED'),
 (SELECT count(*) FROM manual_review_stop_loss_evaluations WHERE case_id=$1),
 (SELECT count(*) FROM manual_review_case_events WHERE case_id=$1 AND event_type='STOP_LOSS_EVALUATED'),
 (SELECT count(*) FROM outbox_events WHERE aggregate_id=$1 AND event_type='token.manual-review.stop-loss-evaluated.v1'),
 ARRAY(SELECT to_owner_id FROM manual_review_assignments WHERE case_id=$1 ORDER BY escalation_level),
 ARRAY(SELECT aggregate_version FROM outbox_events WHERE aggregate_id=$1 AND event_type='token.manual-review.sla-escalated.v1' ORDER BY aggregate_version),
 (SELECT row_to_json(u)::text FROM usage_requests u WHERE id=$2),
 (SELECT health_version FROM routing_policies WHERE id=$3)
 FROM manual_review_cases WHERE id=$1`, id, usage.ID, f.route).
				Scan(&level, &revision, &escalated, &evaluations, &stopLossEvents, &stopLossOutbox, &assigned, &versions, &usageAfter, &healthVersion)
			if err != nil {
				t.Fatal(err)
			}
			if level != wantLevel || revision != wantLevel || escalated != wantLevel ||
				!slices.Equal(assigned, owners[:wantLevel]) || len(versions) != wantLevel {
				t.Fatalf("level=%d revision=%d events=%d owners=%v outbox=%v, want %d escalations", level, revision, escalated, assigned, versions, wantLevel)
			}
			for i, version := range versions {
				if version != i+1 {
					t.Fatalf("unexpected outbox versions %v", versions)
				}
			}
			if evaluations != wantStopLoss || stopLossEvents != wantStopLoss || stopLossOutbox != wantStopLoss || healthVersion-routeBefore != int64(wantStopLoss) {
				t.Fatalf("stop-loss evidence=%d/%d/%d, route health version change=%d, want %d", evaluations, stopLossEvents, stopLossOutbox, healthVersion-routeBefore, wantStopLoss)
			}
			if usageAfter != usageBefore {
				t.Fatal("SLA escalation changed the usage receipt")
			}
		}
		return f, id, schedule, owners, check
	}

	// Hold the case row while both workers pass the real candidate SELECT.
	// pg_stat_activity proves they reached the UPDATE with that same candidate.
	runBlocked := func(t *testing.T, f financialRouteFixture, id uuid.UUID, schedule manualreview.OnCallSchedule, workers int, beforeCommit func(pgx.Tx)) int {
		t.Helper()
		ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
		defer cancel()
		tx, err := f.r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(f.ctx)
		if _, err = tx.Exec(ctx, `SELECT id FROM manual_review_cases WHERE id=$1 FOR UPDATE`, id); err != nil {
			t.Fatal(err)
		}
		type outcome struct {
			count int
			err   error
		}
		done := make(chan outcome, workers)
		for range workers {
			go func() {
				count, err := f.r.EscalateOverdueManualReviews(ctx, schedule, 1)
				done <- outcome{count, err}
			}()
		}
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			var waiting int
			if err := f.r.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
 WHERE datname=current_database() AND wait_event_type='Lock'
 AND query LIKE 'UPDATE manual_review_cases%SET escalation_level=%'`).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting == workers {
				break
			}
			select {
			case <-ticker.C:
			case <-ctx.Done():
				t.Fatal("workers did not reach the blocked escalation UPDATE")
			}
		}
		if beforeCommit != nil {
			beforeCommit(tx)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		total := 0
		for range workers {
			select {
			case result := <-done:
				if result.err != nil {
					t.Fatal(result.err)
				}
				total += result.count
			case <-ctx.Done():
				t.Fatal("SLA worker did not finish")
			}
		}
		return total
	}

	t.Run("two_workers_and_strict_one_hour_boundary", func(t *testing.T) {
		f, id, schedule, _, check := fixture(t)
		now := f.r.now().Truncate(time.Microsecond)
		f.r.now = func() time.Time { return now }
		if n := runBlocked(t, f, id, schedule, 2, nil); n != 1 {
			t.Fatalf("concurrent escalations=%d, want 1", n)
		}
		check(1, 1)
		first := now
		for _, window := range []struct {
			elapsed time.Duration
			count   int
		}{
			{time.Hour - time.Microsecond, 0},
			{time.Hour, 0},
			{time.Hour + time.Microsecond, 1},
		} {
			now = first.Add(window.elapsed)
			if n, err := f.r.EscalateOverdueManualReviews(f.ctx, schedule, 1); err != nil || n != window.count {
				t.Fatalf("elapsed %v: escalations=%d, err=%v, want %d", window.elapsed, n, err, window.count)
			}
			check(1+window.count, 1)
		}
	})

	for _, change := range []struct {
		name, assignments string
		count             int
	}{
		{"closed_before_write", "status='RESOLVED',resolved_at=$2", 0},
		{"deadlines_completed_before_write", "claimed_at=$2,first_response_at=$2,mitigation_at=$2,resolution_due_at=$2::timestamptz+interval '1 hour'", 0},
		{"current_owner_and_stage_after_lock", "claimed_at=$2,first_response_at=NULL,first_response_due_at=$2,mitigation_at=$2,resolution_due_at=$2::timestamptz+interval '1 hour'", 1},
	} {
		t.Run(change.name, func(t *testing.T) {
			f, id, schedule, owners, check := fixture(t)
			now, previousOwner := f.r.now(), uuid.New()
			f.r.now = func() time.Time { return now }
			n := runBlocked(t, f, id, schedule, 1, func(tx pgx.Tx) {
				t.Helper()
				if _, err := tx.Exec(f.ctx, `UPDATE manual_review_cases SET updated_at=$2,owner_id=$3,`+change.assignments+` WHERE id=$1`, id, now, previousOwner); err != nil {
					t.Fatal(err)
				}
			})
			if n != change.count {
				t.Fatalf("escalations=%d, want %d", n, change.count)
			}
			check(change.count, 0)
			if change.count == 1 {
				var current, from uuid.UUID
				var assignmentStage, eventStage, outboxStage string
				err := f.r.pool.QueryRow(f.ctx, `SELECT c.owner_id,a.from_owner_id,a.reason,e.details->>'stage',o.payload->>'stage'
 FROM manual_review_cases c
 JOIN manual_review_assignments a ON a.case_id=c.id
 JOIN manual_review_case_events e ON e.case_id=c.id AND e.event_type='SLA_ESCALATED'
 JOIN outbox_events o ON o.aggregate_id=c.id AND o.event_type='token.manual-review.sla-escalated.v1'
 WHERE c.id=$1`, id).Scan(&current, &from, &assignmentStage, &eventStage, &outboxStage)
				const stage = "FIRST_RESPONSE_OVERDUE"
				if err != nil || current != owners[0] || from != previousOwner || assignmentStage != stage || eventStage != stage || outboxStage != stage {
					t.Fatalf("assignment and events missed current row: owner=%v from=%v stage=%q/%q/%q err=%v", current, from, assignmentStage, eventStage, outboxStage, err)
				}
			}
		})
	}

	t.Run("outbox_failure_rolls_back_and_retries", func(t *testing.T) {
		f, _, schedule, _, check := fixture(t)
		config, err := pgx.ParseConfig(os.Getenv("PINVOU_TEST_TOKEN_DATABASE_URL"))
		if err != nil {
			t.Fatal(err)
		}
		// This database was created and is owned by the cifixture above.
		config.Database = f.r.pool.Config().ConnConfig.Database
		admin, err := pgx.ConnectConfig(f.ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		defer admin.Close(context.Background())
		if _, err := admin.Exec(f.ctx, `
 CREATE FUNCTION fixture_fail_sla_outbox() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN
   RAISE EXCEPTION 'controlled SLA outbox failure';
 END $$;
 CREATE TRIGGER fixture_fail_sla_outbox BEFORE INSERT ON outbox_events FOR EACH ROW
 WHEN (NEW.event_type='token.manual-review.sla-escalated.v1') EXECUTE FUNCTION fixture_fail_sla_outbox()
`); err != nil {
			t.Fatal(err)
		}
		n, err := f.r.EscalateOverdueManualReviews(f.ctx, schedule, 1)
		var pgErr *pgconn.PgError
		if n != 0 || !errors.As(err, &pgErr) || pgErr.Message != "controlled SLA outbox failure" {
			t.Fatalf("escalations=%d err=%v, want controlled outbox failure", n, err)
		}
		check(0, 0)
		if _, err := admin.Exec(f.ctx, `DROP TRIGGER fixture_fail_sla_outbox ON outbox_events`); err != nil {
			t.Fatal(err)
		}
		if n, err := f.r.EscalateOverdueManualReviews(f.ctx, schedule, 1); err != nil || n != 1 {
			t.Fatalf("retry escalations=%d err=%v, want one success", n, err)
		}
		check(1, 1)
	})
}
