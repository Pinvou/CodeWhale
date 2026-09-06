package token

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/tokengovernance"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

type natsActivationReader struct {
	sync.Mutex
	state string
	epoch int64
}

func (r *natsActivationReader) Snapshot(context.Context) (tokengovernance.Snapshot, error) {
	r.Lock()
	defer r.Unlock()
	return tokengovernance.Snapshot{State: r.state, Epoch: r.epoch}, nil
}
func (r *natsActivationReader) set(state string, epoch int64) {
	r.Lock()
	defer r.Unlock()
	r.state, r.epoch = state, epoch
}

func TestTokenApprovalConsumersNATSIntegration(t *testing.T) {
	rawURL := os.Getenv("PINVOU_TEST_NATS_URL")
	if rawURL == "" {
		t.Skip("dedicated test-only NATS JetStream URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	broker, err := events.NewNATSBroker(ctx, rawURL, "activation-integration")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(broker.Close)
	connection, err := nats.Connect(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(connection.Close)
	stream, err := jetstream.New(connection)
	if err != nil {
		t.Fatal(err)
	}
	testStream, err := stream.Stream(ctx, "ACCOUNT_PLATFORM")
	if err != nil {
		t.Fatal(err)
	}
	// PINVOU_TEST_NATS_URL is explicitly a disposable dedicated JetStream;
	// no production/shared NATS URL may be supplied to this integration gate.
	if err = testStream.Purge(ctx); err != nil {
		t.Fatal(err)
	}
	durables := []string{"token-model-lifecycle-executor-v1", "token-catalog-rollback-executor-v1", "token-credential-revoke-executor-v1"}
	for _, durable := range durables {
		_ = stream.DeleteConsumer(ctx, "ACCOUNT_PLATFORM", durable)
	}
	t.Cleanup(func() {
		for _, durable := range durables {
			_ = stream.DeleteConsumer(context.Background(), "ACCOUNT_PLATFORM", durable)
		}
	})
	var reads, posts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			reads.Add(1)
			w.WriteHeader(404)
			return
		}
		posts.Add(1)
		w.WriteHeader(503)
	}))
	defer server.Close()
	reader := &natsActivationReader{state: "PRE_ACTIVATION"}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	finished := make(chan error, 1)
	go func() {
		finished <- RunTokenApprovalConsumers(runCtx, broker, reader, server.URL, server.URL, "test-service-token")
	}()
	wait := func(label string, check func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !check() {
			if time.Now().After(deadline) {
				t.Fatal(label)
			}
			select {
			case err := <-finished:
				t.Fatalf("supervisor exited: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	time.Sleep(40 * time.Millisecond)
	for _, durable := range durables {
		if _, err = stream.Consumer(ctx, "ACCOUNT_PLATFORM", durable); err == nil {
			t.Fatal("PRE registered approved consumers")
		}
	}
	reader.set("ACTIVE", 1)
	wait("three ACTIVE durable consumers not registered", func() bool {
		for _, durable := range durables {
			consumer, err := stream.Consumer(ctx, "ACCOUNT_PLATFORM", durable)
			if err != nil {
				return false
			}
			info, err := consumer.Info(ctx)
			if err != nil || info.Config.MaxDeliver != -1 || info.Config.FilterSubject != "account.approval-command.authorized.v1" {
				return false
			}
		}
		return true
	})
	publish := func(command string) {
		t.Helper()
		policy, err := approvalpolicy.Lookup(command)
		if err != nil {
			t.Fatal(err)
		}
		id := uuid.New()
		payload, _ := json.Marshal(tokenAuthorizedEvent{ApprovalRequestID: id, CommandType: command, CommandFingerprint: strings.Repeat("a", 64), PolicyVersion: policy.PolicyVersion, ExecutorIdentity: policy.ExecutorIdentity, ExecutionDeadline: time.Now().Add(time.Hour), NormalizedCommand: json.RawMessage(`{}`), TargetResourceType: "TEST_RESOURCE", TargetResourceID: uuid.NewString(), CriticalResourceVersions: json.RawMessage(`{}`), FinancialSnapshot: json.RawMessage(`{}`)})
		event := events.Event{ID: uuid.New(), Type: "account.approval-command.authorized.v1", AggregateID: id, AggregateVersion: 1, TraceID: "nats-activation-test", OccurredAt: time.Now(), Payload: payload}
		body, err := event.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if err = broker.Publish(ctx, event.Type, body, event.ID.String()); err != nil {
			t.Fatal(err)
		}
	}
	publish("WALLET_ADJUSTMENT")
	wait("valid other-domain authorization was not acknowledged", func() bool {
		for _, durable := range durables {
			consumer, err := stream.Consumer(ctx, "ACCOUNT_PLATFORM", durable)
			if err != nil {
				return false
			}
			info, err := consumer.Info(ctx)
			if err != nil || info.Delivered.Consumer < 1 || info.NumAckPending != 0 {
				return false
			}
		}
		return true
	})
	if reads.Load() != 0 || posts.Load() != 0 {
		t.Fatal("non-target event called Token or Account")
	}
	publish("TOKEN_CREDENTIAL_REVOKE")
	wait("unconfirmed target was not retained and retried", func() bool { return reads.Load() >= 2 })
	credentialConsumer, err := stream.Consumer(ctx, "ACCOUNT_PLATFORM", durables[2])
	if err != nil {
		t.Fatal(err)
	}
	info, err := credentialConsumer.Info(ctx)
	if err != nil || info.NumAckPending == 0 {
		t.Fatalf("unknown event was dropped: %+v %v", info, err)
	}
	reader.set("SAFE_ROLLBACK", 2)
	time.Sleep(500 * time.Millisecond)
	before := reads.Load()
	time.Sleep(400 * time.Millisecond)
	if reads.Load() != before {
		t.Fatal("SAFE consumer continued claiming work")
	}
	reader.set("ACTIVE", 3)
	wait("resumed epoch did not retain the original pending event", func() bool { return reads.Load() > before })
	stop()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("approval consumers did not stop")
	}
}
