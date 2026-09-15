package events

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// This fixture verifies transport semantics, not Account's durable attempt or
// domain-effect protocol. Those require the separate real-PG consumer tests.
func TestAuthorizationRawConsumerNATSIntegration(t *testing.T) {
	binary := os.Getenv("PINVOU_TEST_NATS_SERVER_BINARY")
	if binary == "" {
		t.Skip("explicit isolated NATS binary required")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("fixture requires an absolute NATS binary")
	}
	root := t.TempDir()
	ca, certificate, key := natsFixtureTLS(t)
	for name, body := range map[string][]byte{"ca.crt": ca, "server.crt": certificate, "server.key": key} {
		if os.WriteFile(filepath.Join(root, name), body, 0600) != nil {
			t.Fatal("fixture TLS material")
		}
	}
	durable := approvalconsumer.LegacyBilling().Durable
	seeds := map[string]string{}
	users := []map[string]any{}
	for _, role := range []string{"fixture-admin", "billing-command-executor", "fixture-no-ack"} {
		seed, public := natsFixtureSeed(t, root, role)
		seeds[role] = seed
		user := map[string]any{"nkey": public}
		if role != "fixture-admin" {
			publish := []string{"$JS.API.STREAM.INFO." + platformStream,
				"$JS.API.CONSUMER.INFO." + platformStream + "." + durable,
				"$JS.API.CONSUMER.MSG.NEXT." + platformStream + "." + durable,
				"account-platform.dead-letter." + authorizationSubject}
			if role != "fixture-no-ack" {
				publish = append(publish, "$JS.ACK."+platformStream+"."+durable+".>")
			}
			user["permissions"] = map[string]any{"publish": publish, "subscribe": []string{"_INBOX.billing-command-executor.>"}}
		}
		users = append(users, user)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("fixture loopback port")
	}
	address := listener.Addr().String()
	listener.Close()
	config := map[string]any{"listen": address, "jetstream": map[string]any{"store_dir": filepath.Join(root, "data")},
		"tls":           map[string]any{"cert_file": filepath.Join(root, "server.crt"), "key_file": filepath.Join(root, "server.key"), "min_version": "1.2"},
		"authorization": map[string]any{"users": users}}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(config) != nil || os.WriteFile(filepath.Join(root, "server.json"), encoded.Bytes(), 0600) != nil {
		t.Fatal("fixture configuration")
	}
	if exec.Command(binary, "-t", "-c", filepath.Join(root, "server.json")).Run() != nil {
		t.Fatal("fixture server configuration validation")
	}
	process := exec.Command(binary, "-c", filepath.Join(root, "server.json"))
	process.Stdout, process.Stderr = io.Discard, io.Discard
	if process.Start() != nil {
		t.Fatal("fixture startup")
	}
	t.Cleanup(func() { _ = process.Process.Kill(); _ = process.Wait() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var admin *nats.Conn
	var js jetstream.JetStream
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		admin, js, err = connectNATS("tls://"+address, "fixture-admin", NATSConfig{Environment: "production-fixture", CAFile: filepath.Join(root, "ca.crt"), SeedFile: seeds["fixture-admin"]})
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || admin == nil {
		t.Fatal("fixture readiness")
	}
	t.Cleanup(admin.Close)
	stream, err := js.CreateStream(ctx, platformStreamConfig())
	if err != nil {
		t.Fatal(err)
	}
	consumerConfig, err := authorizationConsumerConfig(approvalconsumer.LegacyBilling(), 0)
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := js.CreateConsumer(ctx, platformStream, consumerConfig)
	if err != nil {
		t.Fatal(err)
	}
	info, err := consumer.Info(ctx)
	if err != nil || info.Config.AckWait != 30*time.Second || len(info.Config.BackOff) != 0 || info.Config.MaxDeliver != -1 || info.Config.MaxAckPending != approvalconsumer.MaxAckPending {
		t.Fatal("server's effective authorization configuration differs")
	}
	newBroker := func(seed string) *NATSBroker {
		t.Helper()
		broker, err := NewNATSBroker(ctx, "tls://"+address, "billing-command-executor", NATSConfig{Environment: "production-fixture", CAFile: filepath.Join(root, "ca.crt"), SeedFile: seed})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(broker.Close)
		return broker
	}
	broker := newBroker(seeds["billing-command-executor"])
	noAck := newBroker(seeds["fixture-no-ack"])
	// Only this fixture's stream gets a shortened duplicate window, after the
	// production constructor check. This is not production resource readiness.
	shortWindow := platformStreamConfig()
	shortWindow.Duplicates = 100 * time.Millisecond
	if _, err := js.UpdateStream(ctx, shortWindow); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 16)
	var mu sync.Mutex
	seen := map[string]bool{}
	logicalDispatches := 0
	sequences := []uint64{}
	var escaped AuthorizationRawDelivery
	stop, err := broker.ConsumeAuthorization(ctx, durable, func(ctx context.Context, delivery AuthorizationRawDelivery) {
		mu.Lock()
		if !seen[delivery.MessageID()] {
			seen[delivery.MessageID()] = true
			logicalDispatches++
		}
		sequences = append(sequences, delivery.Metadata().StreamSequence)
		escaped = delivery
		mu.Unlock()
		results <- delivery.DoubleAck(ctx)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	waitResult := func() error {
		t.Helper()
		select {
		case result := <-results:
			return result
		case <-ctx.Done():
			t.Fatal("transport result deadline")
			return ctx.Err()
		}
	}
	// Deliberately not an Event: proves raw transport never enters generic
	// decoding. The business decoder is independently required before effects.
	body := []byte(`{"raw": 9007199254740993, "preserve":"<>&"}`)
	id := uuid.NewString()
	first, err := js.Publish(ctx, authorizationSubject, body, jetstream.WithMsgID(id))
	if err != nil || waitResult() != nil {
		t.Fatal("first server-confirmed ACK failed")
	}
	duplicate, err := js.Publish(ctx, authorizationSubject, body, jetstream.WithMsgID(id))
	if err != nil || !duplicate.Duplicate || duplicate.Sequence != first.Sequence {
		t.Fatal("same-window duplicate was not suppressed")
	}
	time.Sleep(150 * time.Millisecond)
	second, err := js.Publish(ctx, authorizationSubject, body, jetstream.WithMsgID(id))
	if err != nil || second.Duplicate || second.Sequence == first.Sequence || waitResult() != nil {
		t.Fatal("second physical sequence was not independently confirmed")
	}
	stop()
	mu.Lock()
	if logicalDispatches != 1 || len(sequences) != 2 || sequences[0] != first.Sequence || sequences[1] != second.Sequence || !bytes.Equal(escaped.DeliveryBytes(), body) {
		t.Error("raw bytes or per-sequence transport identity lost")
	}
	mu.Unlock()
	if err := escaped.DoubleAck(ctx); !errors.Is(err, ErrAuthorizationDelivery) {
		t.Fatal("retained callback handle remained active")
	}
	info, err = consumer.Info(ctx)
	if err != nil || info.NumAckPending != 0 || info.NumPending != 0 {
		t.Fatal("confirmed ACKs did not settle broker pending state")
	}
	// A cancelled ACK must not dispatch; NAK keeps the same original message,
	// and a subsequent delivery can obtain a real server confirmation.
	deliveries := 0
	stopRecovery, err := broker.ConsumeAuthorization(ctx, durable, func(ctx context.Context, delivery AuthorizationRawDelivery) {
		deliveries++
		if deliveries == 1 {
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			if !errors.Is(delivery.DoubleAck(cancelled), context.Canceled) {
				results <- errors.New("cancelled ACK did not fail")
				return
			}
			if err := delivery.NakWithDelay(10 * time.Millisecond); err != nil {
				results <- err
			}
			return
		}
		ack, err := delivery.PublishDeadLetter(ctx)
		if err == nil {
			stored, readErr := stream.GetMsg(ctx, ack.StreamSequence)
			if readErr != nil || !bytes.Equal(stored.Data, body) || stored.Header.Get(nats.MsgIdHdr) != delivery.MessageID()+":"+durable+":dead-letter" {
				err = errors.New("DLQ original bytes or fixed identity lost")
			}
		}
		if err == nil {
			err = delivery.DoubleAck(ctx)
		}
		results <- err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stopRecovery)
	if _, err := js.Publish(ctx, authorizationSubject, body, jetstream.WithMsgID(uuid.NewString())); err != nil || waitResult() != nil {
		t.Fatal("NAK/redelivery/DLQ PubAck/DoubleAck recovery failed")
	}
	stopRecovery()
	if deliveries != 2 {
		t.Fatal("recovery was not original-message redelivery")
	}
	stopNoAck, err := noAck.ConsumeAuthorization(ctx, durable, func(ctx context.Context, delivery AuthorizationRawDelivery) {
		bounded, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		results <- delivery.DoubleAck(bounded)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stopNoAck)
	if _, err := js.Publish(ctx, authorizationSubject, body, jetstream.WithMsgID(uuid.NewString())); err != nil {
		t.Fatal(err)
	}
	if err := waitResult(); err == nil {
		t.Fatal("server-denied ACK was reported as confirmed")
	}
	stopNoAck()
	info, err = consumer.Info(ctx)
	if err != nil || info.NumAckPending != 1 {
		t.Fatal(fmt.Sprintf("unconfirmed ACK did not retain original pending message: %v", err))
	}
}
