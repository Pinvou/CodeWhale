package token

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
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
	binary := os.Getenv("PINVOU_TEST_NATS_SERVER_BINARY")
	if binary == "" {
		t.Skip("explicit isolated NATS binary required")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("fixture requires an absolute NATS binary")
	}
	root := t.TempDir()
	ca, certificate, key := tokenNATSFixtureTLS(t)
	for name, body := range map[string][]byte{"ca.crt": ca, "server.crt": certificate, "server.key": key} {
		if err := os.WriteFile(filepath.Join(root, name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	configuration := map[string]any{
		"listen": address, "jetstream": map[string]any{"store_dir": filepath.Join(root, "data")},
		"tls": map[string]any{"cert_file": filepath.Join(root, "server.crt"), "key_file": filepath.Join(root, "server.key"), "min_version": "1.2"},
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(configuration) != nil || os.WriteFile(filepath.Join(root, "server.json"), encoded.Bytes(), 0600) != nil {
		t.Fatal("fixture configuration")
	}
	if err := exec.Command(binary, "-t", "-c", filepath.Join(root, "server.json")).Run(); err != nil {
		t.Fatal("fixture server configuration")
	}
	process := exec.Command(binary, "-c", filepath.Join(root, "server.json"))
	process.Stdout, process.Stderr = io.Discard, io.Discard
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Process.Kill(); _ = process.Wait() })

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var connection *nats.Conn
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		connection, err = nats.Connect("tls://"+address, nats.RootCAs(filepath.Join(root, "ca.crt")), nats.Timeout(time.Second))
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || connection == nil {
		t.Fatal("fixture readiness")
	}
	t.Cleanup(connection.Close)
	stream, err := jetstream.New(connection)
	if err != nil {
		t.Fatal(err)
	}
	_, err = stream.CreateStream(ctx, jetstream.StreamConfig{
		Name: "ACCOUNT_PLATFORM", Description: "Pinvou account platform domain events",
		Subjects: []string{"account.>", "billing.>", "token.>", "account-platform.dead-letter.>"},
		Storage:  jetstream.FileStorage, Retention: jetstream.LimitsPolicy, MaxAge: 30 * 24 * time.Hour, Duplicates: 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	durables := []string{"token-model-lifecycle-executor-v1", "token-catalog-rollback-executor-v1", "token-credential-revoke-executor-v1"}
	for _, durable := range durables {
		if _, err = stream.CreateConsumer(ctx, "ACCOUNT_PLATFORM", jetstream.ConsumerConfig{
			Name: durable, Durable: durable, FilterSubject: approvalconsumer.AuthorizationSubject,
			AckPolicy: jetstream.AckExplicitPolicy, AckWait: 30 * time.Second, MaxDeliver: -1, MaxAckPending: approvalconsumer.MaxAckPending,
		}); err != nil {
			t.Fatal(err)
		}
	}
	broker, err := events.NewNATSBroker(ctx, "tls://"+address, approvalconsumer.TokenWorkload, events.NATSConfig{
		Environment: "test", ResourceMode: "provisioned", CAFile: filepath.Join(root, "ca.crt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(broker.Close)
	var reads, posts atomic.Int64
	var routes sync.Map
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			reads.Add(1)
			if strings.HasSuffix(r.URL.Path, "/route") {
				parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
				if len(parts) >= 2 {
					if route, ok := routes.Load(parts[len(parts)-2]); ok {
						_ = json.NewEncoder(w).Encode(route)
						return
					}
				}
			}
			w.WriteHeader(404)
			return
		}
		posts.Add(1)
		w.WriteHeader(503)
	}))
	defer server.Close()
	publish := func(command string) {
		t.Helper()
		policy, err := approvalpolicy.Lookup(command)
		if err != nil {
			t.Fatal(err)
		}
		id := uuid.New()
		targetID := uuid.NewString()
		targetType := "PROVIDER_CREDENTIAL"
		tokenAccountID := uuid.NewString()
		normalized := json.RawMessage(fmt.Sprintf(`{"token_account_id":%q,"expected_account_version":3,"credential_id":%q,"credential_version":2}`, tokenAccountID, targetID))
		critical := json.RawMessage(fmt.Sprintf(`{"token_account_id":%q,"token_account_version":3,"credential_id":%q,"credential_version":2,"credential_status":"ACTIVE","credential_fingerprint":%q,"active_route_set_fingerprint":%q,"provisioning_lease_set_fingerprint":%q,"inflight_request_set_fingerprint":%q,"replacement_credential_version":null,"policy_version":1}`, tokenAccountID, targetID, strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("d", 64), strings.Repeat("e", 64)))
		financial := json.RawMessage(`{}`)
		if command == "WALLET_ADJUSTMENT" {
			targetType = "MANUAL_ADJUSTMENT"
			walletID := uuid.NewString()
			normalized = json.RawMessage(fmt.Sprintf(`{"adjustment_id":%q,"wallet_id":%q,"amount_microcredits":5,"requested_by":%q}`, targetID, walletID, uuid.NewString()))
			critical = json.RawMessage(`{"adjustment_version":1}`)
			financial = json.RawMessage(fmt.Sprintf(`{"wallet_id":%q,"delta_microcredits":5}`, walletID))
		}
		payload, err := json.Marshal(tokenAuthorizedEvent{ApprovalRequestID: id, CommandType: command, CommandFingerprint: strings.Repeat("a", 64), PolicyVersion: policy.PolicyVersion, ExecutorIdentity: policy.ExecutorIdentity, ExecutionDeadline: time.Now().UTC().Add(time.Hour), NormalizedCommand: normalized, TargetResourceType: targetType, TargetResourceID: targetID, CriticalResourceVersions: critical, FinancialSnapshot: financial})
		if err != nil {
			t.Fatal(err)
		}
		event := events.Event{ID: uuid.New(), Type: approvalconsumer.AuthorizationSubject, AggregateID: id, AggregateVersion: 1, TraceID: "nats-activation-test", OccurredAt: time.Now().UTC(), Payload: payload}
		body, err := event.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := approvalencoding.DecodeAuthorizationEvent(body, approvalencoding.ExpectedAuthorizationBinding{ApprovedRequestVersion: 1, ApprovalEpoch: 1}); err != nil {
			t.Fatalf("invalid %s authorization fixture: %v", command, err)
		}
		digest := sha256.Sum256(body)
		descriptor := approvalconsumer.LegacyBilling()
		if policy.ExecutorIdentity == approvalconsumer.TokenWorkload {
			for _, candidate := range approvalconsumer.TokenCommands() {
				if candidate.CommandType == command {
					descriptor = candidate
					break
				}
			}
		}
		routes.Store(event.ID.String(), tokenAuthorizationRoute{EventID: event.ID, ApprovalRequestID: id,
			ApprovedRequestVersion: 1, ApprovalEpoch: 1, SerializationVersion: approvalencoding.LegacySerializationVersion, Provenance: "FROZEN_AT_APPROVAL",
			EventDigest: hex.EncodeToString(digest[:]), DigestKind: "DELIVERY_BYTES", CommandType: command,
			PolicyVersion: policy.PolicyVersion, ExecutorIdentity: policy.ExecutorIdentity,
			CanonicalWorkload: descriptor.Workload, CanonicalDurable: descriptor.Durable})
		if err = broker.Publish(ctx, event.Type, body, event.ID.String()); err != nil {
			t.Fatal(err)
		}
	}
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
	publish("WALLET_ADJUSTMENT")
	time.Sleep(300 * time.Millisecond)
	for _, durable := range durables {
		consumer, consumerErr := stream.Consumer(ctx, "ACCOUNT_PLATFORM", durable)
		if consumerErr != nil {
			t.Fatal(consumerErr)
		}
		info, infoErr := consumer.Info(ctx)
		if infoErr != nil || info.Delivered.Consumer != 0 {
			t.Fatalf("PRE consumed authorization work: durable=%s delivered=%d err=%v", durable, info.Delivered.Consumer, infoErr)
		}
	}
	if reads.Load() != 0 || posts.Load() != 0 {
		t.Fatalf("PRE invoked authorization handlers: reads=%d posts=%d", reads.Load(), posts.Load())
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
	if reads.Load() != 3 || posts.Load() != 0 {
		t.Fatalf("non-target event did not use route-only verification reads=%d posts=%d", reads.Load(), posts.Load())
	}
	beforeTarget := reads.Load()
	publish("TOKEN_CREDENTIAL_REVOKE")
	wait("unconfirmed target was not retained and retried", func() bool { return reads.Load() >= beforeTarget+4 })
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

func tokenNATSFixtureTLS(t *testing.T) ([]byte, []byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "isolated Token NATS fixture"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ = x509.ParseCertificate(der)
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour)}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	return tokenPEMBytes("CERTIFICATE", der), tokenPEMBytes("CERTIFICATE", leafDER), tokenPEMBytes("PRIVATE KEY", private)
}

func tokenPEMBytes(kind string, body []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: body})
}
