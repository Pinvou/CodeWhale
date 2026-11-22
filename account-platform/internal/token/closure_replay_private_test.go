package token

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"testing"
	"time"
)

// This fixture uses the public pinned Token migrator and actual runtime role.
// It does not exercise the Account worker or external issuer.
func TestClosureReplayDurableFence(t *testing.T) {
	r, ctx := retainedManagementTokenRepository(t)
	t.Run("no-prior-usage-keeps-permanent-marker", func(t *testing.T) {
		user := uuid.New()
		if err := r.AnonymizeUsageReplay(ctx, user); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM usage_replay_anonymized_users WHERE user_id=$1`, user).Scan(&n); err != nil || n != 1 {
			t.Fatalf("durable marker count=%d error=%v", n, err)
		}
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if err = lockReplayUserTx(ctx, tx, user); !errors.Is(err, ErrForbidden) {
			t.Fatalf("late new-usage admission fence=%v", err)
		}
	})
	t.Run("payload-cleared-and-late-writer-refused", func(t *testing.T) {
		user := uuid.New()
		id := retainedCancellationUsage(t, r, ctx, user, uuid.New(), nil, "RECEIVED")
		if _, err := r.pool.Exec(ctx, `UPDATE usage_requests SET status='RELEASED',settlement_status='RELEASED',execution_status='FAILED',cancel_status='LOCAL_CONFIRMED' WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
		if err := r.AnonymizeUsageReplay(ctx, user); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM usage_dispatch_preparations WHERE usage_request_id=$1`, id).Scan(&n); err != nil || n != 0 {
			t.Fatal("dispatch payload remains", err)
		}
		err := r.SaveReplayResult(ctx, id, ReplayResult{Kind: "JSON", HTTPStatus: 200, ContentType: "application/json", Payload: []byte(`{"late":"private"}`)})
		if !errors.Is(err, ErrForbidden) {
			t.Fatalf("actual late SaveReplayResult=%v", err)
		}
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM usage_replay_results WHERE usage_request_id=$1`, id).Scan(&n); err != nil || n != 0 {
			t.Fatal("late replay recreated", err)
		}
	})
	t.Run("unsafe-financial-state-never-acknowledged", func(t *testing.T) {
		user := uuid.New()
		retainedCancellationUsage(t, r, ctx, user, uuid.New(), nil, "RECEIVED")
		if err := r.AnonymizeUsageReplay(ctx, user); !errors.Is(err, ErrForbidden) {
			t.Fatalf("unsafe cleanup=%v", err)
		}
		var n int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM usage_replay_anonymized_users WHERE user_id=$1`, user).Scan(&n); err != nil || n != 0 {
			t.Fatal("unsafe marker published", err)
		}
	})
	t.Run("cleanup-waits-for-active-payload-writer", func(t *testing.T) {
		user := uuid.New()
		writer, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer writer.Rollback(ctx)
		if err = lockReplayUserTx(ctx, writer, user); err != nil {
			t.Fatal(err)
		}
		started := make(chan struct{})
		done := make(chan error, 1)
		go func() { close(started); done <- r.AnonymizeUsageReplay(ctx, user) }()
		<-started
		deadline := time.Now().Add(3 * time.Second)
		waiting := false
		for time.Now().Before(deadline) {
			if err = r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event='advisory' AND query LIKE '%pg_advisory_xact_lock(hashtextextended%')`).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting {
				break
			}
			select {
			case e := <-done:
				t.Fatalf("cleanup completed while writer locked: %v", e)
			default:
			}
			time.Sleep(time.Millisecond)
		}
		if !waiting {
			t.Fatal("database did not show cleanup blocked on user fence")
		}
		if err = writer.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case err = <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(context.Cause(ctx))
		}
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if err = lockReplayUserTx(ctx, tx, user); !errors.Is(err, ErrForbidden) {
			t.Fatalf("post-cleanup writer not fenced: %v", err)
		}
	})
}
