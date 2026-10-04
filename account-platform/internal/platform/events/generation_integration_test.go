package events

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func appendGenerationFixture(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := AppendOutbox(ctx, tx, "account.membership.changed.v1", uuid.New(), 1, map[string]any{"status": "ACTIVE"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var id uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT event_id FROM outbox_events`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func claimGenerationFixture(t *testing.T, store *PostgresStore, lease time.Duration) Event {
	t.Helper()
	events, err := store.Claim(context.Background(), "same-worker", 1, lease)
	if err != nil || len(events) != 1 {
		t.Fatalf("claim count=%d error=%v", len(events), err)
	}
	return events[0]
}

func rejectOldAcknowledgements(t *testing.T, store *PostgresStore, old Event) {
	t.Helper()
	ctx := context.Background()
	if err := store.MarkPublished(ctx, "same-worker", old); err == nil {
		t.Fatal("stale publication acknowledged new lease")
	}
	if err := store.MarkFailed(ctx, "same-worker", old, time.Now().Add(time.Hour), "stale failure"); err == nil {
		t.Fatal("stale failure modified new lease")
	}
	if err := store.MarkDeadLettered(ctx, "same-worker", old, "stale dead letter"); err == nil {
		t.Fatal("stale dead letter modified new lease")
	}
}

func TestPostgresPublicationGenerationFencesRequeueAcrossAllRelayDomains(t *testing.T) {
	for _, domain := range []string{"account", "billing", "token"} {
		t.Run(domain, func(t *testing.T) {
			pool := testdb.Open(t, domain)
			store := &PostgresStore{pool: pool}
			id := appendGenerationFixture(t, pool)
			old := claimGenerationFixture(t, store, time.Minute)
			if old.PublicationGeneration != 0 || old.LeaseUntil.IsZero() {
				t.Fatal("initial durable publication metadata missing")
			}
			before, err := old.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Requeue(context.Background(), id, "fixture approved replay"); err != nil {
				t.Fatal(err)
			}
			current := claimGenerationFixture(t, store, time.Minute)
			rejectOldAcknowledgements(t, store, old)
			after, err := current.Marshal()
			if err != nil || string(before) != string(after) || current.ID != old.ID || current.PublicationGeneration != 1 || current.Attempts != 0 {
				t.Fatal("replay changed business envelope or did not replace publication identity")
			}
			if err := store.MarkPublished(context.Background(), "same-worker", current); err != nil {
				t.Fatal(err)
			}
			var attempts, generation int64
			if err := pool.QueryRow(context.Background(), `SELECT attempts,publication_generation FROM outbox_events WHERE event_id=$1 AND published_at IS NOT NULL AND dead_lettered_at IS NULL`, id).Scan(&attempts, &generation); err != nil || attempts != 1 || generation != 1 {
				t.Fatal("stale acknowledgements changed new generation state")
			}
		})
	}
}

func TestPostgresPublicationGenerationMigrationDownAndUp(t *testing.T) {
	for _, domain := range []string{"account", "billing", "token"} {
		t.Run(domain, func(t *testing.T) {
			pool := testdb.Open(t, domain)
			ctx := context.Background()
			id := appendGenerationFixture(t, pool)
			loaded, err := migrations.Load(filepath.Join("..", "..", "..", "migrations", domain))
			if err != nil {
				t.Fatal(err)
			}
			connection, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Release()
			if err := migrations.DownOne(ctx, connection.Conn(), loaded); err != nil {
				t.Fatal(err)
			}
			migrations.Unlock(ctx, connection.Conn())
			var count int
			if err := connection.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='outbox_events' AND column_name='publication_generation'`).Scan(&count); err != nil || count != 0 {
				t.Fatal("rollback did not remove publication generation")
			}
			if err := migrations.Up(ctx, connection.Conn(), loaded); err != nil {
				t.Fatal(err)
			}
			migrations.Unlock(ctx, connection.Conn())
			var generation int64
			if err := connection.QueryRow(ctx, `SELECT publication_generation FROM outbox_events WHERE event_id=$1`, id).Scan(&generation); err != nil || generation != 0 {
				t.Fatal("migration reapplication did not preserve original business event")
			}
		})
	}
}

func TestPostgresConcurrentRequeueAdvancesGenerationAtomically(t *testing.T) {
	pool := testdb.Open(t, "account")
	store := &PostgresStore{pool: pool}
	id := appendGenerationFixture(t, pool)
	old := claimGenerationFixture(t, store, time.Minute)
	var workers sync.WaitGroup
	errorsSeen := make(chan error, 8)
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			errorsSeen <- store.Requeue(context.Background(), id, "fixture concurrent approved replay")
		}()
	}
	workers.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	current := claimGenerationFixture(t, store, time.Minute)
	if current.PublicationGeneration != 8 || current.ID != old.ID {
		t.Fatal("concurrent replay lost or reused a generation")
	}
	rejectOldAcknowledgements(t, store, old)
	if err := store.MarkPublished(context.Background(), "same-worker", current); err != nil {
		t.Fatal(err)
	}
	for _, args := range []struct {
		id     uuid.UUID
		reason string
	}{{uuid.Nil, "valid reason"}, {id, " "}, {uuid.New(), "not found"}} {
		if err := store.Requeue(context.Background(), args.id, args.reason); err == nil {
			t.Fatal("invalid replay unexpectedly succeeded")
		}
	}
	var generation int64
	if err := pool.QueryRow(context.Background(), `SELECT publication_generation FROM outbox_events WHERE event_id=$1`, id).Scan(&generation); err != nil || generation != 8 {
		t.Fatal("rejected replay changed publication generation")
	}
}

func TestPostgresExpiredAndReplacedLeaseCannotAcknowledgeSameGeneration(t *testing.T) {
	pool := testdb.Open(t, "account")
	store := &PostgresStore{pool: pool}
	appendGenerationFixture(t, pool)
	old := claimGenerationFixture(t, store, time.Second)
	// Real lease expiry, without changing database timestamps or generation.
	time.Sleep(1100 * time.Millisecond)
	rejectOldAcknowledgements(t, store, old)
	current := claimGenerationFixture(t, store, time.Minute)
	if old.PublicationGeneration != current.PublicationGeneration || old.LeaseUntil.Equal(current.LeaseUntil) {
		t.Fatal("ordinary lease retry replaced publication generation or reused lease")
	}
	rejectOldAcknowledgements(t, store, old)
	if err := store.MarkPublished(context.Background(), "same-worker", current); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresFailedPublishKeepsGenerationAcrossRetry(t *testing.T) {
	pool := testdb.Open(t, "account")
	store := &PostgresStore{pool: pool}
	appendGenerationFixture(t, pool)
	first := claimGenerationFixture(t, store, time.Minute)
	if err := store.MarkFailed(context.Background(), "same-worker", first, time.Now().Add(-time.Second), "fixture transport failure"); err != nil {
		t.Fatal(err)
	}
	retry := claimGenerationFixture(t, store, time.Minute)
	if publicationID(first) != publicationID(retry) || retry.Attempts != 1 {
		t.Fatal("ordinary transport retry changed durable publication identity")
	}
	rejectOldAcknowledgements(t, store, first)
	if err := store.MarkDeadLettered(context.Background(), "same-worker", retry, "fixture exhausted delivery"); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.Claim(context.Background(), "same-worker", 1, time.Minute)
	if err != nil || len(claimed) != 0 {
		t.Fatal("dead-lettered publication remained claimable")
	}
	if err := store.Requeue(context.Background(), retry.ID, "fixture restore after DLQ"); err != nil {
		t.Fatal(err)
	}
	current := claimGenerationFixture(t, store, time.Minute)
	if current.PublicationGeneration != retry.PublicationGeneration+1 {
		t.Fatal("dead letter replay reused delivery identity")
	}
	if err := store.MarkPublished(context.Background(), "same-worker", current); err != nil {
		t.Fatal(err)
	}
}

type lostDeadLetterAck struct {
	OutboxStore
	lost bool
}

func (store *lostDeadLetterAck) MarkDeadLettered(ctx context.Context, worker string, event Event, message string) error {
	if !store.lost {
		store.lost = true
		return errors.New("fixture lost database acknowledgement after DLQ publish")
	}
	return store.OutboxStore.MarkDeadLettered(ctx, worker, event, message)
}

type primaryFailBroker struct {
	Broker
	primary string
}

func (broker primaryFailBroker) Publish(ctx context.Context, subject string, body []byte, id string) error {
	if subject == broker.primary {
		return errors.New("fixture primary transport unavailable")
	}
	return broker.Broker.Publish(ctx, subject, body, id)
}

func TestPostgresNATSDLQRetryDeduplicatesUntilManualReplay(t *testing.T) {
	broker, subject := natsFixture(t, true)
	pool := testdb.Open(t, "account")
	store := &PostgresStore{pool: pool}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := AppendOutbox(ctx, tx, subject, uuid.New(), 1, map[string]any{"status": "ACTIVE"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	loss := &lostDeadLetterAck{OutboxStore: store}
	relay := Relay{Store: loss, Broker: primaryFailBroker{Broker: broker, primary: subject}, WorkerID: "fixture-relay", Lease: time.Second, MaxAttempts: 1}
	if _, err := relay.RunOnce(ctx); err == nil {
		t.Fatal("fixture did not lose DLQ database acknowledgement")
	}
	time.Sleep(1100 * time.Millisecond)
	if result, err := relay.RunOnce(ctx); err != nil || result.DeadLettered != 1 {
		t.Fatal("DLQ lost ACK retry did not complete")
	}
	stream, err := broker.stream.Stream(ctx, broker.streamName)
	if err != nil {
		t.Fatal(err)
	}
	info, err := stream.Info(ctx)
	if err != nil || info.State.Msgs != 1 || info.Config.Duplicates != 24*time.Hour {
		t.Fatal("DLQ lost ACK retry bypassed unchanged 24h deduplication")
	}
	var id uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT event_id FROM outbox_events WHERE dead_lettered_at IS NOT NULL`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := store.Requeue(ctx, id, "fixture approved DLQ replay"); err != nil {
		t.Fatal(err)
	}
	if result, err := relay.RunOnce(ctx); err != nil || result.DeadLettered != 1 {
		t.Fatal("replayed generation did not reach DLQ")
	}
	info, err = stream.Info(ctx)
	if err != nil || info.State.Msgs != 2 {
		t.Fatal("24h deduplication suppressed new DLQ generation")
	}
	first, err := stream.GetMsg(ctx, info.State.FirstSeq)
	if err != nil {
		t.Fatal(err)
	}
	second, err := stream.GetMsg(ctx, info.State.LastSeq)
	if err != nil || string(first.Data) != string(second.Data) || first.Header.Get("Nats-Msg-Id") == second.Header.Get("Nats-Msg-Id") {
		t.Fatal("DLQ replay changed business data or reused message ID")
	}
}
