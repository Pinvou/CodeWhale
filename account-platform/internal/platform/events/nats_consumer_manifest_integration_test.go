package events

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// This captures Q only on a newly owned loopback fixture. The private binding
// is injected by this same-package test, not made available to runtime callers.
func TestApprovalConsumerQPlusOneNATSIntegration(t *testing.T) {
	binary := os.Getenv("PINVOU_TEST_NATS_SERVER_BINARY")
	if binary == "" {
		t.Skip("explicit isolated NATS binary required")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("fixture binary must be absolute")
	}
	root := t.TempDir()
	ca, cert, key := natsFixtureTLS(t)
	for name, body := range map[string][]byte{"ca.crt": ca, "server.crt": cert, "server.key": key} {
		if os.WriteFile(filepath.Join(root, name), body, 0600) != nil {
			t.Fatal("fixture TLS")
		}
	}
	keys, seeds := map[string]string{}, map[string]string{}
	for _, role := range fixedNATSV2RoleNames() {
		seeds[role], keys[role] = natsFixtureSeed(t, root, role)
	}
	previousDigest := consumerManifestHash([]byte("fixture historical artifact not configured"))
	current, err := json.Marshal(map[string]any{"version": 2, "purpose": "pinvou-nats-service-identities", "public_keys": keys,
		"previous_v1_sha256": previousDigest, "overlap_expires_at": time.Now().UTC().Add(time.Hour).Truncate(time.Second).Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	identityHash := consumerManifestHash(current)
	manifest, err := ApprovalConsumerManifest(identityHash)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := RenderNATSAuthorizationWithConsumers(current, nil, manifest, identityHash, consumerManifestHash(manifest))
	if err != nil {
		t.Fatal(err)
	}
	var policy struct {
		Authorization struct {
			Users []json.RawMessage `json:"users"`
		} `json:"authorization"`
	}
	if json.Unmarshal(rendered.Configuration, &policy) != nil {
		t.Fatal("fixture rendered ACL")
	}
	adminSeed, adminKey := natsFixtureSeed(t, root, "fixture-admin")
	adminUser, _ := json.Marshal(map[string]string{"nkey": adminKey})
	policy.Authorization.Users = append(policy.Authorization.Users, adminUser)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	configuration := map[string]any{"listen": address, "jetstream": map[string]any{"store_dir": filepath.Join(root, "data")},
		"tls": map[string]any{"cert_file": filepath.Join(root, "server.crt"), "key_file": filepath.Join(root, "server.key"), "min_version": "1.2"}, "authorization": policy.Authorization}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	configPath := filepath.Join(root, "server.json")
	if encoder.Encode(configuration) != nil || os.WriteFile(configPath, buffer.Bytes(), 0600) != nil {
		t.Fatal("fixture config")
	}
	if output, err := exec.Command(binary, "-t", "-c", configPath).CombinedOutput(); err != nil {
		t.Fatalf("fixture config validation: %s", output)
	}
	process := exec.Command(binary, "-c", configPath)
	process.Stdout, process.Stderr = io.Discard, io.Discard
	if process.Start() != nil {
		t.Fatal("fixture start")
	}
	t.Cleanup(func() { _ = process.Process.Kill(); _ = process.Wait() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var admin *nats.Conn
	var js jetstream.JetStream
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		admin, js, err = connectNATS("tls://"+address, "fixture-admin", NATSConfig{Environment: "production-fixture", CAFile: filepath.Join(root, "ca.crt"), SeedFile: adminSeed})
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || admin == nil {
		t.Fatal("fixture ready")
	}
	t.Cleanup(admin.Close)
	stream, err := js.CreateStream(ctx, platformStreamConfig())
	if err != nil {
		t.Fatal(err)
	}
	oldID := uuid.NewString()
	if _, err := js.Publish(ctx, authorizationSubject, []byte("old"), jetstream.WithMsgID(oldID)); err != nil {
		t.Fatal(err)
	}
	streamInfo, err := stream.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	q := streamInfo.State.LastSeq
	if q != 1 {
		t.Fatal("fixture Q")
	}
	binding := &ApprovalConsumerResourceBinding{startSequences: map[string]uint64{}}
	for _, descriptor := range approvalconsumer.All() {
		var start uint64
		if descriptor.Workload == approvalconsumer.BillingWorkload {
			start = q + 1
			binding.startSequences[descriptor.Durable] = start
		}
		config, err := authorizationConsumerConfig(descriptor, start)
		if err != nil {
			t.Fatal(err)
		}
		consumer, err := js.CreateConsumer(ctx, platformStream, config)
		if err != nil {
			t.Fatal(err)
		}
		info, err := consumer.Info(ctx)
		if err != nil || !validAuthorizationConsumerConfig(info.Config, config) {
			t.Fatal("actual resource differs")
		}
		if start != 0 && (info.NumPending != 0 || info.NumAckPending != 0) {
			t.Fatal("Q+1 consumed historical messages")
		}
	}
	newBroker := func(role string, resources *ApprovalConsumerResourceBinding) *NATSBroker {
		t.Helper()
		broker, err := NewNATSBroker(ctx, "tls://"+address, role, NATSConfig{Environment: "production-fixture", CAFile: filepath.Join(root, "ca.crt"), SeedFile: seeds[role], ApprovalConsumerResources: resources})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(broker.Close)
		return broker
	}
	billing := newBroker(approvalconsumer.BillingWorkload, binding)
	unbound := newBroker(approvalconsumer.BillingWorkload, nil)
	token := newBroker(approvalconsumer.TokenWorkload, nil)
	for _, descriptor := range approvalconsumer.BillingCommands() {
		if _, err := unbound.ConsumeAuthorization(ctx, descriptor.Durable, func(context.Context, AuthorizationRawDelivery) {}); !errors.Is(err, ErrNATSResources) {
			t.Fatal("missing binding started consumer")
		}
		if _, err := token.ConsumeAuthorization(ctx, descriptor.Durable, func(context.Context, AuthorizationRawDelivery) {}); !errors.Is(err, ErrNATSResources) {
			t.Fatal("wrong workload started consumer")
		}
	}
	newID := uuid.NewString()
	results := make(chan error, 3)
	for _, descriptor := range approvalconsumer.BillingCommands() {
		stop, err := billing.ConsumeAuthorization(ctx, descriptor.Durable, func(ctx context.Context, delivery AuthorizationRawDelivery) {
			if delivery.MessageID() != newID || delivery.Metadata().StreamSequence != q+1 {
				results <- errors.New("historical or wrong sequence delivered")
				return
			}
			results <- delivery.DoubleAck(ctx)
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(stop)
	}
	if _, err := js.Publish(ctx, authorizationSubject, []byte("new"), jetstream.WithMsgID(newID)); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("Q+1 delivery deadline")
		}
	}
	// Existing Token durables retain their historical start and can consume
	// both physical records, but cannot acquire Billing's DLQ capability.
	tokenResults := make(chan error, 6)
	for _, descriptor := range approvalconsumer.TokenCommands() {
		stop, err := token.ConsumeAuthorization(ctx, descriptor.Durable, func(ctx context.Context, delivery AuthorizationRawDelivery) {
			if delivery.MessageID() != oldID && delivery.MessageID() != newID {
				tokenResults <- errors.New("unexpected Token delivery")
				return
			}
			if _, err := delivery.PublishDeadLetter(ctx); !errors.Is(err, ErrAuthorizationDelivery) {
				tokenResults <- errors.New("Token acquired DLQ capability")
				return
			}
			tokenResults <- delivery.DoubleAck(ctx)
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(stop)
	}
	for range 6 {
		select {
		case err := <-tokenResults:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("Token delivery deadline")
		}
	}
	// Bypass the Go registry in these negative probes. The actual rendered
	// broker ACL, not a local helper rejection or a timeout, must deny access.
	deny := func(connection *nats.Conn, subject string) {
		t.Helper()
		denied := make(chan struct{}, 1)
		connection.SetErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
			if strings.Contains(err.Error(), "Permissions Violation") {
				select {
				case denied <- struct{}{}:
				default:
				}
			}
		})
		if connection.Publish(subject, []byte(`{}`)) != nil || connection.FlushTimeout(time.Second) != nil {
			t.Fatal("ACL probe transport failed")
		}
		select {
		case <-denied:
		case <-time.After(time.Second):
			t.Fatal("broker did not explicitly deny cross-workload ACL")
		}
	}
	for _, descriptor := range approvalconsumer.BillingCommands() {
		deny(token.connection, "$JS.API.CONSUMER.INFO."+platformStream+"."+descriptor.Durable)
	}
	for _, descriptor := range approvalconsumer.TokenCommands() {
		deny(billing.connection, "$JS.API.CONSUMER.INFO."+platformStream+"."+descriptor.Durable)
	}
	deny(token.connection, "account-platform.dead-letter."+authorizationSubject)
	deny(token.connection, authorizationSubject)
	deny(billing.connection, authorizationSubject)
}
