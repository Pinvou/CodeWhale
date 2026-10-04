package events

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

type ackProbeJetStream struct {
	jetstream.JetStream
	failures atomic.Int64
}

type lostOutboxAck struct {
	OutboxStore
	lost bool
}

func (store *lostOutboxAck) MarkPublished(ctx context.Context, worker string, event Event) error {
	if !store.lost {
		store.lost = true
		return errors.New("fixture lost database acknowledgement after NATS publish")
	}
	return store.OutboxStore.MarkPublished(ctx, worker, event)
}

func TestPostgresNATSRelayLostAckAndManualReplayPreserveOneInboxEffect(t *testing.T) {
	broker, subject := natsFixture(t, true)
	pool := testdb.Open(t, "account")
	store := &PostgresStore{pool: pool}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `CREATE TABLE fixture_event_effects(aggregate_id UUID PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	var deliveries atomic.Int64
	stop, err := broker.Consume(ctx, "consumer", subject, func(ctx context.Context, event Event) error {
		_, err := store.ProcessInbox(ctx, event, func(ctx context.Context, tx pgx.Tx, event Event) error {
			_, err := tx.Exec(ctx, `INSERT INTO fixture_event_effects(aggregate_id) VALUES($1)`, event.AggregateID)
			return err
		})
		if err == nil {
			deliveries.Add(1)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	aggregate := uuid.New()
	if err := AppendOutbox(ctx, tx, subject, aggregate, 1, map[string]any{"membership_status": "ACTIVE"}); err != nil {
		t.Fatal(err)
	}
	loss := &lostOutboxAck{OutboxStore: store}
	relay := Relay{Store: loss, Broker: broker, WorkerID: "fixture-relay", Lease: time.Second}
	if result, err := relay.RunOnce(ctx); err != nil || result.Claimed != 0 {
		t.Fatal("relay published uncommitted business event")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := relay.RunOnce(ctx); err == nil {
		t.Fatal("fixture did not lose acknowledgement")
	}
	waitFor(t, func() bool {
		var count int
		err := pool.QueryRow(ctx, `SELECT count(*) FROM fixture_event_effects`).Scan(&count)
		return err == nil && count == 1
	})
	// Keep the real lease and 24h NATS deduplication semantics. Only the fixture
	// lease is short; no database row is requeued or modified to trigger retry.
	time.Sleep(1100 * time.Millisecond)
	if result, err := relay.RunOnce(ctx); err != nil || result.Published != 1 {
		t.Fatalf("retry published=%d error=%v", result.Published, err)
	}
	stream, err := broker.stream.Stream(ctx, broker.streamName)
	if err != nil {
		t.Fatal(err)
	}
	info, err := stream.Info(ctx)
	if err != nil || info.State.Msgs != 1 || info.Config.Duplicates != 24*time.Hour {
		t.Fatal("lost ACK retry bypassed NATS deduplication")
	}
	stats, err := store.Stats(ctx)
	if err != nil || stats.Pending != 0 {
		t.Fatal("published retry left outbox pending")
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM fixture_event_effects`).Scan(&count); err != nil || count != 1 {
		t.Fatal("consumer effect duplicated")
	}
	var id uuid.UUID
	var generation int64
	if err := pool.QueryRow(ctx, `SELECT event_id,publication_generation FROM outbox_events`).Scan(&id, &generation); err != nil || generation != 0 {
		t.Fatal("ordinary lost ACK retry changed publication generation")
	}
	if err := store.Requeue(ctx, id, "fixture approved replay inside 24h window"); err != nil {
		t.Fatal(err)
	}
	if result, err := relay.RunOnce(ctx); err != nil || result.Published != 1 {
		t.Fatal("manual replay did not publish new generation")
	}
	waitFor(t, func() bool { return deliveries.Load() == 2 })
	info, err = stream.Info(ctx)
	if err != nil || info.State.Msgs != 2 {
		t.Fatal("24h deduplication suppressed manually replayed generation")
	}
	first, err := stream.GetMsg(ctx, info.State.FirstSeq)
	if err != nil {
		t.Fatal(err)
	}
	second, err := stream.GetMsg(ctx, info.State.LastSeq)
	if err != nil || string(first.Data) != string(second.Data) || first.Header.Get(nats.MsgIdHdr) != id.String()+":0" || second.Header.Get(nats.MsgIdHdr) != id.String()+":1" {
		t.Fatal("manual replay changed envelope or reused original publication identity")
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM fixture_event_effects`).Scan(&count); err != nil || count != 1 {
		t.Fatal("replayed event duplicated business effect")
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM inbox_messages`).Scan(&count); err != nil || count != 1 {
		t.Fatal("publication generation changed business Inbox identity")
	}
}

type ackProbeConsumer struct {
	jetstream.Consumer
	failures *atomic.Int64
}
type ackProbeMessage struct {
	jetstream.Msg
	failures *atomic.Int64
}

func (m ackProbeMessage) NakWithDelay(delay time.Duration) error {
	err := m.Msg.NakWithDelay(delay)
	if err != nil {
		m.failures.Add(1)
	}
	return err
}
func (c ackProbeConsumer) Consume(handler jetstream.MessageHandler, opts ...jetstream.PullConsumeOpt) (jetstream.ConsumeContext, error) {
	return c.Consumer.Consume(func(m jetstream.Msg) { handler(ackProbeMessage{m, c.failures}) }, opts...)
}
func (j *ackProbeJetStream) CreateOrUpdateConsumer(ctx context.Context, stream string, config jetstream.ConsumerConfig) (jetstream.Consumer, error) {
	// Accelerate broker ack-timeout redelivery as well as the client's NAK delay.
	// The business limit and production handler/ack/dead-letter code are unchanged.
	config.BackOff = []time.Duration{10 * time.Millisecond}
	config.AckWait = 10 * time.Millisecond
	consumer, err := j.JetStream.CreateOrUpdateConsumer(ctx, stream, config)
	if err != nil {
		return nil, err
	}
	return ackProbeConsumer{consumer, &j.failures}, nil
}

func natsFixture(t *testing.T, deadLetterSubject bool) (*NATSBroker, string) {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv("PINVOU_ACCOUNT_TEST_NATS_URL"))
	if raw == "" {
		t.Skip("PINVOU_ACCOUNT_TEST_NATS_URL not configured; real NATS test not executed")
	}
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint.Scheme != "nats" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		t.Fatal("invalid test NATS endpoint")
	}
	ip := net.ParseIP(endpoint.Hostname())
	if ip == nil || !ip.IsLoopback() {
		t.Fatal("test NATS must address a literal loopback host")
	}
	connection, err := nats.Connect(raw, nats.Timeout(3*time.Second), nats.NoReconnect())
	if err != nil {
		t.Fatal("connect to test NATS failed")
	}
	js, err := jetstream.New(connection, jetstream.WithDefaultTimeout(500*time.Millisecond))
	if err != nil {
		connection.Close()
		t.Fatal(err)
	}
	name := "A03_TEST_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	subject := "test." + name + ".membership.changed.v1"
	subjects := []string{subject}
	if deadLetterSubject {
		subjects = append(subjects, "account-platform.dead-letter."+subject)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: name, Subjects: subjects, Storage: jetstream.FileStorage, Duplicates: 24 * time.Hour}); err != nil {
		connection.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := js.DeleteStream(ctx, name); err != nil {
			t.Error("remove test-owned NATS stream failed")
		}
		connection.Close()
	})
	return &NATSBroker{connection: connection, stream: js, streamName: name, retry: func(int) time.Duration { return 10 * time.Millisecond }}, subject
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("test NATS state did not converge")
}

func TestNATSRealPublishDeduplicatesAndConsumerAcknowledges(t *testing.T) {
	broker, subject := natsFixture(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var calls atomic.Int64
	stop, err := broker.Consume(ctx, "consumer", subject, func(context.Context, Event) error { calls.Add(1); return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	event := testEvent(0)
	event.Type = subject
	body, err := event.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := broker.Publish(ctx, subject, body, event.ID.String()); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() bool { return calls.Load() == 1 })
	stream, err := broker.stream.Stream(ctx, broker.streamName)
	if err != nil {
		t.Fatal(err)
	}
	info, err := stream.Info(ctx)
	if err != nil || info.State.Msgs != 1 {
		t.Fatalf("message count=%v error=%v", info, err)
	}
	consumer, err := broker.stream.Consumer(ctx, broker.streamName, "consumer")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { info, err := consumer.Info(ctx); return err == nil && info.NumAckPending == 0 })
}

func TestNATSConsumerRetriesDeadLetterTransportAfterBusinessLimit(t *testing.T) {
	// Actual NATS initially has no DLQ subject. Client retry scheduling and broker
	// ack-timeout backoff are reduced to 10ms; business attempt count remains 12.
	broker, subject := natsFixture(t, false)
	probe := &ackProbeJetStream{JetStream: broker.stream}
	broker.stream = probe
	var retryCalls atomic.Int64
	broker.retry = func(int) time.Duration { retryCalls.Add(1); return 10 * time.Millisecond }
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var calls atomic.Int64
	stop, err := broker.Consume(ctx, "consumer", subject, func(context.Context, Event) error { calls.Add(1); return errors.New("fixture consumer failure") })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	event := testEvent(0)
	event.Type = subject
	body, _ := event.Marshal()
	if err := broker.Publish(ctx, subject, body, publicationID(event)); err != nil {
		t.Fatal(err)
	}
	consumer, err := broker.stream.Consumer(ctx, broker.streamName, "consumer")
	if err != nil {
		t.Fatal(err)
	}
	var delivered uint64
	defer func() {
		if t.Failed() {
			t.Logf("business_calls=%d delivered=%d retry_calls=%d nak_errors=%d", calls.Load(), delivered, retryCalls.Load(), probe.failures.Load())
		}
	}()
	waitFor(t, func() bool {
		info, err := consumer.Info(ctx)
		if err == nil {
			delivered = info.Delivered.Consumer
		}
		return err == nil && info.Delivered.Consumer > 12
	})
	if calls.Load() != 12 {
		t.Fatalf("business handler calls=%d want=12", calls.Load())
	}
	stream, err := broker.stream.Stream(ctx, broker.streamName)
	if err != nil {
		t.Fatal(err)
	}
	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	configuration := info.Config
	configuration.Subjects = append(configuration.Subjects, "account-platform.dead-letter."+subject)
	if _, err := broker.stream.UpdateStream(ctx, configuration); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { info, err := stream.Info(ctx); return err == nil && info.State.Msgs == 2 })
	waitFor(t, func() bool { info, err := consumer.Info(ctx); return err == nil && info.NumAckPending == 0 })
	if calls.Load() != 12 {
		t.Fatalf("DLQ retries invoked business handler again: %d", calls.Load())
	}
	// A new transport generation must also receive its own consumer DLQ identity.
	// The envelope and business event ID remain identical to the original message.
	event.PublicationGeneration = 1
	if err := broker.Publish(ctx, subject, body, publicationID(event)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { info, err := stream.Info(ctx); return err == nil && info.State.Msgs == 4 })
	waitFor(t, func() bool { info, err := consumer.Info(ctx); return err == nil && info.NumAckPending == 0 })
	if calls.Load() != 24 {
		t.Fatal("new generation did not get exactly twelve business attempts")
	}
	info, err = stream.Info(ctx)
	if err != nil || info.Config.Duplicates != 24*time.Hour {
		t.Fatal("consumer replay changed NATS duplicate window")
	}
	deadLetterIDs := make(map[string]bool)
	for sequence := info.State.FirstSeq; sequence <= info.State.LastSeq; sequence++ {
		message, err := stream.GetMsg(ctx, sequence)
		if err != nil {
			t.Fatal(err)
		}
		if message.Subject == "account-platform.dead-letter."+subject {
			if string(message.Data) != string(body) {
				t.Fatal("consumer DLQ changed business envelope")
			}
			deadLetterIDs[message.Header.Get(nats.MsgIdHdr)] = true
		}
	}
	if len(deadLetterIDs) != 2 || !deadLetterIDs[event.ID.String()+":0:consumer-dead-letter"] || !deadLetterIDs[event.ID.String()+":1:consumer-dead-letter"] {
		t.Fatal("consumer DLQ suppressed or duplicated a publication generation")
	}
}
