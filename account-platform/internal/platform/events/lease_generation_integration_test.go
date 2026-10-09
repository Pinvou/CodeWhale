package events

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This verifies the actual ordinary outbox and inbox transaction substrate.
// It neither enables audited manual replay nor substitutes for a real broker.
func TestPostgresOrdinaryDeliveryLeaseAndGeneration(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_EVENTS_DATABASE_URL")
	if raw == "" {
		t.Skip("PINVOU_TEST_EVENTS_DATABASE_URL is required for actual PostgreSQL")
	}
	ctx, done := context.WithTimeout(context.Background(), 45*time.Second)
	defer done()
	control, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close(context.Background())
	schema := "ordinary_outbox_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := control.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := control.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Errorf("cleanup own schema: %v", err)
		}
	}()
	if _, err := control.Exec(ctx, "SET search_path TO "+quoted); err != nil {
		t.Fatal(err)
	}
	all, err := migrations.Load("../../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	var substrate []migrations.Migration
	for _, migration := range all {
		if migration.Version == 1 || migration.Version == 3 {
			substrate = append(substrate, migration)
		}
	}
	if len(substrate) != 2 {
		t.Fatal("ordinary outbox migrations are not installed")
	}
	if err := migrations.Up(ctx, control, substrate); err != nil {
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	store := &PostgresStore{pool: pool}
	appendOne := func() Event {
		t.Helper()
		aggregate := uuid.New()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := AppendOutbox(ctx, tx, "account.user.updated", aggregate, 1, map[string]any{"user_id": aggregate}); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		claimed, err := store.Claim(ctx, "same-worker", 1, time.Second, []string{"account.>", "fixture.>"})
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claim=%v err=%v", claimed, err)
		}
		return claimed[0]
	}
	snapshot := func(id uuid.UUID) string {
		t.Helper()
		var body string
		if err := pool.QueryRow(ctx, `SELECT to_jsonb(row)::text FROM outbox_events row WHERE event_id=$1`, id).Scan(&body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	for _, tc := range []struct {
		name string
		mark func(Event) error
	}{
		{"published", func(e Event) error { return store.MarkPublished(ctx, "same-worker", e) }},
		{"failed", func(e Event) error {
			return store.MarkFailed(ctx, "same-worker", e, time.Now().Add(time.Hour), "fixture-failure")
		}},
		{"dead-lettered", func(e Event) error { return store.MarkDeadLettered(ctx, "same-worker", e, "fixture-failure") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := appendOne()
			before := snapshot(event.ID)
			timer := time.NewTimer(time.Until(event.LeaseUntil) + 100*time.Millisecond)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				t.Fatal(ctx.Err())
			}
			if err := tc.mark(event); err == nil {
				t.Fatal("expired lease acknowledged before reclaim")
			}
			if after := snapshot(event.ID); after != before {
				t.Fatal("rejected expired acknowledgement changed persisted facts")
			}
			reclaimed, err := store.Claim(ctx, "same-worker", 1, time.Minute, []string{"account.>", "fixture.>"})
			if err != nil || len(reclaimed) != 1 {
				t.Fatalf("reclaim=%v err=%v", reclaimed, err)
			}
			fresh := reclaimed[0]
			if fresh.ID != event.ID || fresh.PublicationGeneration != event.PublicationGeneration || fresh.LeaseUntil.Equal(event.LeaseUntil) || publicationID(fresh) != publicationID(event) {
				t.Fatal("ordinary retry changed logical publication or reused expired lease")
			}
			before = snapshot(event.ID)
			if err := tc.mark(event); err == nil {
				t.Fatal("old same-worker lease acknowledged newer claim")
			}
			wrongGeneration := fresh
			wrongGeneration.PublicationGeneration++
			if err := tc.mark(wrongGeneration); err == nil {
				t.Fatal("wrong generation acknowledged current lease")
			}
			if after := snapshot(event.ID); after != before {
				t.Fatal("stale acknowledgements changed newer claim")
			}
			if err := tc.mark(fresh); err != nil {
				t.Fatalf("live exact lease rejected: %v", err)
			}
			var attempts int
			if err := pool.QueryRow(ctx, `SELECT attempts FROM outbox_events WHERE event_id=$1`, event.ID).Scan(&attempts); err != nil || attempts != 1 {
				t.Fatalf("attempts=%d err=%v", attempts, err)
			}
		})
	}
	t.Run("failed-retry-retains-generation-and-envelope", func(t *testing.T) {
		event := appendOne()
		if err := store.MarkFailed(ctx, "same-worker", event, time.Now(), "fixture-failure"); err != nil {
			t.Fatal(err)
		}
		claimed, err := store.Claim(ctx, "same-worker", 1, time.Minute, []string{"account.>", "fixture.>"})
		if err != nil || len(claimed) != 1 {
			t.Fatalf("retry=%v err=%v", claimed, err)
		}
		fresh := claimed[0]
		oldBody, _ := event.Marshal()
		newBody, _ := fresh.Marshal()
		if fresh.Attempts != 1 || fresh.PublicationGeneration != event.PublicationGeneration || publicationID(fresh) != publicationID(event) || string(oldBody) != string(newBody) {
			t.Fatal("transport retry changed generation or business envelope")
		}
		if err := store.MarkPublished(ctx, "same-worker", fresh); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("invalid-claim-is-error-not-panic", func(t *testing.T) {
		for _, limit := range []int{-1, 0, 1001} {
			if _, err := store.Claim(ctx, "same-worker", limit, time.Minute, []string{"account.>"}); err == nil {
				t.Errorf("accepted limit %d", limit)
			}
		}
	})
	t.Run("partitioned-claim-retains-identical-lease-fence", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, kind := range []string{"fixture.excluded", "fixture.included"} {
			if err := AppendOutbox(ctx, tx, kind, uuid.New(), 1, map[string]any{"fixture": true}); err != nil {
				_ = tx.Rollback(ctx)
				t.Fatal(err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		partition := &PostgresStore{pool: pool, excludedEventTypes: []string{"fixture.excluded"}}
		claimed, err := partition.Claim(ctx, "partition-worker", 10, time.Minute, []string{"fixture.>"})
		if err != nil || len(claimed) != 1 || claimed[0].Type != "fixture.included" || claimed[0].PublicationGeneration != 0 || claimed[0].LeaseUntil.IsZero() {
			t.Fatalf("partition claim=%v err=%v", claimed, err)
		}
		stale := claimed[0]
		stale.LeaseUntil = stale.LeaseUntil.Add(-time.Second)
		before := snapshot(stale.ID)
		if err := partition.MarkPublished(ctx, "partition-worker", stale); err == nil {
			t.Fatal("partition accepted another lease")
		}
		if snapshot(stale.ID) != before {
			t.Fatal("partition stale acknowledgement changed facts")
		}
		if err := partition.MarkPublished(ctx, "partition-worker", claimed[0]); err != nil {
			t.Fatal(err)
		}
		stats, err := partition.Stats(ctx)
		// The prior failed acknowledgement remains pending for its later retry;
		// the excluded fixture must add no pending record to this partition.
		if err != nil || stats.Pending != 1 || stats.DeadLettered != 1 {
			t.Fatalf("partition stats=%+v err=%v", stats, err)
		}
	})
	t.Run("scope-prefilter-is-literal-and-nonmutating", func(t *testing.T) {
		ids := map[string]uuid.UUID{}
		var idsTemp uuid.UUID
		for _, kind := range []string{"account_.foo", "account_.excluded", "account%.foo", "accountx.foo", "billing.foo"} {
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err = AppendOutbox(ctx, tx, kind, uuid.New(), 1, map[string]any{"fixture": true}); err != nil {
				_ = tx.Rollback(ctx)
				t.Fatal(err)
			}
			if err = tx.QueryRow(ctx, `SELECT event_id FROM outbox_events WHERE event_type=$1`, kind).Scan(&idsTemp); err != nil {
				_ = tx.Rollback(ctx)
				t.Fatal(err)
			}
			ids[kind] = idsTemp
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
		}
		before := map[string]string{}
		for kind, id := range ids {
			before[kind] = snapshot(id)
		}
		partition := &PostgresStore{pool: pool, excludedEventTypes: []string{"account_.excluded"}}
		for _, scope := range []string{"account_.>", "account%.>", "accountx.foo"} {
			claimed, err := partition.Claim(ctx, "scope-worker", 20, time.Minute, []string{scope})
			if err != nil || len(claimed) != 1 || !subjectAllowed([]string{scope}, claimed[0].Type) {
				t.Fatalf("scope=%s claimed=%v err=%v", scope, claimed, err)
			}
			if err = partition.MarkPublished(ctx, "scope-worker", claimed[0]); err != nil {
				t.Fatal(err)
			}
		}
		for _, kind := range []string{"account_.excluded", "billing.foo"} {
			if snapshot(ids[kind]) != before[kind] {
				t.Fatalf("out-of-scope/excluded %s changed", kind)
			}
		}
		if _, err := partition.Claim(ctx, "scope-worker", 20, time.Minute, nil); err == nil {
			t.Fatal("empty scope accepted")
		}
	})
	t.Run("inbox-effect-dedup-and-version-gap-rollback", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `CREATE TABLE fixture_business_effect(event_id UUID PRIMARY KEY)`); err != nil {
			t.Fatal(err)
		}
		event := Event{ID: uuid.New(), Type: "account.user.updated", AggregateID: uuid.New(), AggregateVersion: 1, Payload: []byte(`{}`), TraceID: "fixture", OccurredAt: time.Now()}
		handler := func(ctx context.Context, tx pgx.Tx, e Event) error {
			_, err := tx.Exec(ctx, `INSERT INTO fixture_business_effect VALUES($1)`, e.ID)
			return err
		}
		if applied, err := store.ProcessInbox(ctx, event, handler); err != nil || !applied {
			t.Fatalf("first inbox applied=%v err=%v", applied, err)
		}
		if applied, err := store.ProcessInbox(ctx, event, handler); err != nil || applied {
			t.Fatalf("duplicate inbox applied=%v err=%v", applied, err)
		}
		gap := event
		gap.ID = uuid.New()
		gap.AggregateVersion = 3
		if applied, err := store.ProcessInbox(ctx, gap, handler); !errors.Is(err, ErrVersionGap) || applied {
			t.Fatalf("gap applied=%v err=%v", applied, err)
		}
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM inbox_messages WHERE event_id=$1`, gap.ID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("gap acknowledgement leaked count=%d err=%v", count, err)
		}
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM fixture_business_effect`).Scan(&count); err != nil || count != 1 {
			t.Fatalf("effect count=%d err=%v", count, err)
		}
	})
	t.Run("migration-rerun-preserves-generation", func(t *testing.T) {
		if err := migrations.Up(ctx, control, substrate); err != nil {
			t.Fatal(err)
		}
		var invalid int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE publication_generation<>0`).Scan(&invalid); err != nil || invalid != 0 {
			t.Fatalf("ordinary generation mutated count=%d err=%v", invalid, err)
		}
	})
}
