package events

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestPostgresRelayRetainsTransactionAndRetriesWithoutDuplicateInbox(t *testing.T) {
	pool := testdb.Open(t, "account")
	store := &PostgresStore{pool: pool}
	ctx := context.Background()
	transaction, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer transaction.Rollback(ctx)
	id := uuid.New()
	if err := AppendOutbox(ctx, transaction, "account.membership.changed.v1", id, 1, map[string]any{"membership_status": "ACTIVE"}); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events`).Scan(&before); err != nil || before != 0 {
		t.Fatalf("uncommitted outbox visible: %d error=%v", before, err)
	}
	if err := transaction.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.Claim(ctx, "worker-a", 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim count=%d error=%v", len(claimed), err)
	}
	others, err := store.Claim(ctx, "worker-b", 1, time.Minute)
	if err != nil || len(others) != 0 {
		t.Fatalf("leased event claimed twice count=%d error=%v", len(others), err)
	}
	if err := store.MarkPublished(ctx, "worker-b", claimed[0]); err == nil {
		t.Fatal("foreign worker acknowledged lease")
	}
	if err := store.MarkFailed(ctx, "worker-a", claimed[0], time.Now().Add(-time.Second), "fixture transport failure"); err != nil {
		t.Fatal(err)
	}
	claimed, err = store.Claim(ctx, "worker-b", 1, time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].Attempts != 1 {
		t.Fatalf("retry claim=%+v error=%v", claimed, err)
	}
	if applied, err := store.ProcessInbox(ctx, claimed[0], func(context.Context, pgx.Tx, Event) error { return errors.New("fixture handler failure") }); err == nil || applied {
		t.Fatal("failed consumer committed inbox")
	}
	if applied, err := store.ProcessInbox(ctx, claimed[0], func(context.Context, pgx.Tx, Event) error { return nil }); err != nil || !applied {
		t.Fatalf("consumer retry applied=%v error=%v", applied, err)
	}
	if applied, err := store.ProcessInbox(ctx, claimed[0], func(context.Context, pgx.Tx, Event) error { t.Error("duplicate invoked handler"); return nil }); err != nil || applied {
		t.Fatalf("duplicate applied=%v error=%v", applied, err)
	}
	if err := store.MarkPublished(ctx, "worker-b", claimed[0]); err != nil {
		t.Fatal(err)
	}
	stats, err := store.Stats(ctx)
	if err != nil || stats.Pending != 0 || stats.DeadLettered != 0 {
		t.Fatalf("stats=%+v error=%v", stats, err)
	}
}

func TestPostgresInboxVersionGapRollsBackAndMissingEventClosesGap(t *testing.T) {
	pool := testdb.Open(t, "account")
	store := &PostgresStore{pool: pool}
	ctx := context.Background()
	one := testEvent(0)
	two := one
	two.ID = uuid.New()
	two.AggregateVersion = 2
	handler := func(context.Context, pgx.Tx, Event) error { return nil }
	if applied, err := store.ProcessInbox(ctx, two, handler); !errors.Is(err, ErrVersionGap) || applied {
		t.Fatalf("gap applied=%v error=%v", applied, err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM inbox_messages`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("gap persisted inbox count=%d error=%v", count, err)
	}
	for _, event := range []Event{one, two} {
		if applied, err := store.ProcessInbox(ctx, event, handler); err != nil || !applied {
			t.Fatalf("version=%d applied=%v error=%v", event.AggregateVersion, applied, err)
		}
	}
}
