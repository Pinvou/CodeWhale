package events

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Only an explicitly supplied local server binary is started, with new TLS,
// random identities, loopback port and t.TempDir JetStream storage. No existing
// NATS URL or production credentials are accepted by this fixture.
func TestNATSProvisionedTLSIntegration(t *testing.T) {
	for _, mode := range []string{"password", "nkey"} {
		t.Run(mode, func(t *testing.T) { testNATSProvisionedTLSIntegration(t, mode == "nkey") })
	}
}

func testNATSProvisionedTLSIntegration(t *testing.T, useNKey bool) {
	binary := os.Getenv("PINVOU_TEST_NATS_SERVER_BINARY")
	if binary == "" {
		t.Skip("explicit isolated NATS binary required")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("fixture requires absolute binary path")
	}
	t.Setenv("NATS_CREDENTIALS_FILE", "")
	t.Setenv("NATS_NKEY_SEED_FILE", "")
	root := t.TempDir()
	os.Chmod(root, 0700)
	ca, cert, key := natsFixtureTLS(t)
	for name, data := range map[string][]byte{"ca.crt": ca, "server.crt": cert, "server.key": key} {
		if os.WriteFile(filepath.Join(root, name), data, 0600) != nil {
			t.Fatal("fixture TLS write failed")
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("fixture port unavailable")
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	password := uuid.NewString()
	users := []map[string]any{{"user": "fixture-admin", "password": password}}
	info := []string{"$JS.API.STREAM.INFO.ACCOUNT_PLATFORM"}
	for _, source := range []string{"account", "billing", "token"} {
		pub := append(append([]string{}, info...), source+".>", "account-platform.dead-letter."+source+".>")
		users = append(users, map[string]any{"user": source + "-outbox-relay", "password": password, "permissions": map[string]any{"publish": pub, "subscribe": []string{"_INBOX." + source + "-outbox-relay.>"}}})
	}
	for _, role := range []string{"billing-command-executor", "token-command-executor"} {
		pub := append([]string{}, info...)
		for _, consumer := range provisionedConsumers() {
			if !consumerOwnedBy(role, consumer) {
				continue
			}
			pub = append(pub, "$JS.API.CONSUMER.INFO.ACCOUNT_PLATFORM."+consumer.Name, "$JS.API.CONSUMER.MSG.NEXT.ACCOUNT_PLATFORM."+consumer.Name, "$JS.ACK.ACCOUNT_PLATFORM."+consumer.Name+".>")
			if role == "billing-command-executor" {
				pub = append(pub, "account-platform.dead-letter."+consumer.FilterSubject)
			}
		}
		users = append(users, map[string]any{"user": role, "password": password, "permissions": map[string]any{"publish": pub, "subscribe": []string{"_INBOX." + role + ".>"}}})
	}
	seedFiles := map[string]string{}
	if useNKey {
		for _, user := range users {
			role := user["user"].(string)
			file, public := natsFixtureSeed(t, root, role)
			seedFiles[role] = file
			delete(user, "user")
			delete(user, "password")
			user["nkey"] = public
		}
	}
	config := map[string]any{"listen": net.JoinHostPort("127.0.0.1", itoaFixture(port)), "jetstream": map[string]any{"store_dir": filepath.Join(root, "data")}, "tls": map[string]any{"cert_file": filepath.Join(root, "server.crt"), "key_file": filepath.Join(root, "server.key"), "min_version": "1.2"}, "authorization": map[string]any{"users": users}}
	var configJSON bytes.Buffer
	encoder := json.NewEncoder(&configJSON)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(config) != nil {
		t.Fatal("fixture configuration encoding failed")
	}
	raw := configJSON.Bytes()
	configPath := filepath.Join(root, "server.json")
	if os.WriteFile(configPath, raw, 0600) != nil {
		t.Fatal("fixture configuration write failed")
	}
	if diagnostic, err := exec.Command(binary, "-t", "-c", configPath).CombinedOutput(); err != nil {
		safe := strings.NewReplacer(password, "[fixture-secret]", root, "[fixture-directory]").Replace(string(diagnostic))
		if len(safe) > 1500 {
			safe = safe[:1500]
		}
		t.Fatalf("fixture config parser rejected: %s", safe)
	}
	process := exec.Command(binary, "-c", configPath)
	process.Stdout = io.Discard
	process.Stderr = io.Discard
	if process.Start() != nil {
		t.Fatal("fixture NATS failed to start")
	}
	defer func() { process.Process.Kill(); process.Wait() }()
	address := net.JoinHostPort("127.0.0.1", itoaFixture(port))
	// Each connection snapshots its seed path into the signing callback; changing
	// the process-boundary fixture environment cannot change existing clients.
	urlFor := func(role string) string {
		if useNKey {
			t.Setenv("NATS_NKEY_SEED_FILE", seedFiles[role])
			return "tls://" + address
		}
		return "tls://" + role + ":" + password + "@" + address
	}
	cfg := NATSConfig{Environment: "production-new", CAFile: filepath.Join(root, "ca.crt")}
	var admin *nats.Conn
	var js jetstream.JetStream
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		admin, js, err = connectNATS(urlFor("fixture-admin"), "fixture-admin", cfg)
		if err == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err != nil {
		t.Fatal("fixture verified TLS connection unavailable")
	}
	defer admin.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if ProvisionNATSResources(ctx, js, false) == nil {
		t.Fatal("check accepted missing resources")
	}
	if ProvisionNATSResources(ctx, js, true) != nil {
		if stream, err := js.Stream(ctx, platformStream); err == nil {
			t.Logf("isolated stream configuration: %+v", stream.CachedInfo().Config)
		}
		for _, expected := range provisionedConsumers() {
			if consumer, err := js.Consumer(ctx, platformStream, expected.Name); err == nil {
				t.Logf("isolated consumer configuration: %+v", consumer.CachedInfo().Config)
			}
		}
		t.Fatal("create-only initialization failed")
	}
	if ProvisionNATSResources(ctx, js, true) != nil || ProvisionNATSResources(ctx, js, false) != nil {
		t.Fatal("resource replay changed or rejected configuration")
	}
	brokers := map[string]*NATSBroker{}
	for _, role := range []string{"account-outbox-relay", "billing-outbox-relay", "token-outbox-relay", "billing-command-executor", "token-command-executor"} {
		broker, err := NewNATSBroker(ctx, urlFor(role), role, cfg)
		if err != nil {
			t.Fatal("least-privilege broker could not start")
		}
		defer broker.Close()
		brokers[role] = broker
	}
	wrong := cfg
	wrong.CAFile = filepath.Join(root, "wrong.crt")
	wrongCA, _, _ := natsFixtureTLS(t)
	os.WriteFile(wrong.CAFile, wrongCA, 0600)
	if broker, err := NewNATSBroker(ctx, urlFor("account-outbox-relay"), "account-outbox-relay", wrong); err == nil {
		broker.Close()
		t.Fatal("wrong CA accepted")
	}
	if broker, err := NewNATSBroker(ctx, strings.Replace(urlFor("account-outbox-relay"), "tls:", "nats:", 1), "account-outbox-relay", cfg); err == nil {
		broker.Close()
		t.Fatal("plaintext production accepted")
	}

	const subject = "account.approval-command.authorized.v1"
	if brokers["account-outbox-relay"].Publish(ctx, subject, []byte(`{}`), uuid.NewString()) != nil {
		t.Fatal("legitimate publish denied")
	}
	for _, expected := range provisionedConsumers() {
		role := "token-command-executor"
		if strings.HasPrefix(expected.Name, "billing-") {
			role = "billing-command-executor"
		}
		broker := brokers[role]
		consumer, err := checkedConsumer(ctx, broker.stream, expected)
		if err != nil {
			t.Fatal("own consumer lookup denied")
		}
		if expected.FilterSubject != subject {
			if brokers["billing-outbox-relay"].Publish(ctx, expected.FilterSubject, []byte(`{}`), uuid.NewString()) != nil {
				t.Fatal("refund publish denied")
			}
		}
		batch, err := consumer.Fetch(1, jetstream.FetchMaxWait(time.Second))
		if err != nil {
			t.Fatal("own consumer pull denied")
		}
		var message jetstream.Msg
		for m := range batch.Messages() {
			message = m
		}
		if message == nil {
			t.Fatal("consumer received no event")
		}
		if message.Nak() != nil {
			t.Fatal("NAK denied")
		}
		batch, err = consumer.Fetch(1, jetstream.FetchMaxWait(time.Second))
		if err != nil {
			t.Fatal("redelivery pull denied")
		}
		message = nil
		for m := range batch.Messages() {
			message = m
		}
		if message == nil || message.DoubleAck(ctx) != nil {
			t.Fatal("double ACK denied")
		}
	}
	for _, source := range []string{"account", "billing", "token"} {
		if brokers[source+"-outbox-relay"].Publish(ctx, "account-platform.dead-letter."+source+".fixture.v1", []byte(`{}`), uuid.NewString()) != nil {
			t.Fatal("relay DLQ denied")
		}
	}
	for _, expected := range provisionedConsumers()[:2] {
		if brokers["billing-command-executor"].Publish(ctx, "account-platform.dead-letter."+expected.FilterSubject, []byte(`{}`), uuid.NewString()) != nil {
			t.Fatal("billing DLQ denied")
		}
	}
	// Exercise the actual production Consume paths, including error -> NAK
	// redelivery -> successful handler -> DoubleAck, with valid event envelopes.
	delivered := make(chan string, 2)
	event := Event{ID: uuid.New(), Type: subject, AggregateID: uuid.New(), AggregateVersion: 1, Payload: json.RawMessage(`{}`), TraceID: uuid.NewString(), OccurredAt: time.Now()}
	for _, kind := range []string{"billing", "token"} {
		var attempts atomic.Int32
		handler := func(_ context.Context, got Event) error {
			if got.ID != event.ID {
				return errors.New("fixture unexpected event")
			}
			if attempts.Add(1) == 1 {
				return errors.New("fixture retry")
			}
			select {
			case delivered <- kind:
			default:
			}
			return nil
		}
		var stop func()
		var err error
		if kind == "billing" {
			stop, err = brokers["billing-command-executor"].Consume(ctx, "billing-command-executor-v1", subject, handler)
		} else {
			stop, err = brokers["token-command-executor"].ConsumeUntilResolved(ctx, "token-model-lifecycle-executor-v1", subject, handler)
		}
		if err != nil {
			t.Fatal("production consumer start denied")
		}
		defer stop()
	}
	eventBody, err := event.Marshal()
	if err != nil {
		t.Fatal("fixture event invalid")
	}
	if brokers["account-outbox-relay"].Publish(ctx, subject, eventBody, event.ID.String()) != nil {
		t.Fatal("production-path event publish denied")
	}
	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case kind := <-delivered:
			seen[kind] = true
		case <-time.After(5 * time.Second):
			t.Fatal("production consumer did not redeliver and handle")
		}
	}
	connection := brokers["billing-command-executor"].connection
	deniedInbox := make(chan bool, 1)
	connection.SetErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
		if strings.Contains(err.Error(), "Permissions Violation") {
			select {
			case deniedInbox <- true:
			default:
			}
		}
	})
	subscription, err := connection.Subscribe("_INBOX.token-command-executor.>", func(*nats.Msg) { t.Error("cross-role inbox data exposed") })
	if err != nil || connection.Flush() != nil {
		t.Fatal("inbox negative probe transport failed")
	}
	defer subscription.Unsubscribe()
	select {
	case <-deniedInbox:
	case <-time.After(time.Second):
		t.Fatal("cross-role inbox subscription not denied")
	}
	for _, probe := range []struct{ role, subject string }{
		{"account-outbox-relay", "billing.unauthorized.v1"},
		{"account-outbox-relay", "$JS.API.STREAM.UPDATE.ACCOUNT_PLATFORM"},
		{"account-outbox-relay", "$JS.API.STREAM.CREATE.ACCOUNT_PLATFORM"},
		{"account-outbox-relay", "$JS.API.STREAM.MSG.GET.ACCOUNT_PLATFORM"},
		{"billing-command-executor", "$JS.API.CONSUMER.MSG.NEXT.ACCOUNT_PLATFORM.token-model-lifecycle-executor-v1"},
		{"billing-command-executor", "$JS.ACK.ACCOUNT_PLATFORM.token-model-lifecycle-executor-v1.1.1.1.1.0"},
		{"token-command-executor", "$JS.API.CONSUMER.INFO.ACCOUNT_PLATFORM.billing-command-executor-v1"},
		{"token-command-executor", "$JS.API.CONSUMER.CREATE.ACCOUNT_PLATFORM.token-model-lifecycle-executor-v1.account.approval-command.authorized.v1"},
	} {
		denied := make(chan bool, 1)
		connection := brokers[probe.role].connection
		connection.SetErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
			if strings.Contains(err.Error(), "Permissions Violation") {
				select {
				case denied <- true:
				default:
				}
			}
		})
		if connection.Publish(probe.subject, []byte(`{}`)) != nil || connection.Flush() != nil {
			t.Fatal("negative probe transport failed")
		}
		select {
		case <-denied:
		case <-time.After(time.Second):
			t.Fatal("server did not explicitly deny unauthorized operation")
		}
	}
	stream, _ := js.Stream(ctx, platformStream)
	drift := stream.CachedInfo().Config
	drift.MaxAge = 48 * time.Hour
	if _, err := js.UpdateStream(ctx, drift); err != nil {
		t.Fatal("fixture drift setup failed")
	}
	if ProvisionNATSResources(ctx, js, true) == nil {
		t.Fatal("initializer overwrote existing drift")
	}
	stream, _ = js.Stream(ctx, platformStream)
	if stream.CachedInfo().Config.MaxAge != 48*time.Hour {
		t.Fatal("drift silently repaired")
	}
	// Legacy development explicitly retains automatic initialization/update.
	legacyConfig := cfg
	legacyConfig.Environment = "development"
	legacy, err := NewNATSBroker(ctx, urlFor("fixture-admin"), "legacy-development", legacyConfig)
	if err != nil {
		t.Fatal("legacy automatic initialization regressed")
	}
	defer legacy.Close()
	stopLegacy, err := legacy.Consume(ctx, "legacy-fixture-consumer", "token.fixture.v1", func(context.Context, Event) error { return nil })
	if err != nil {
		t.Fatal("legacy automatic consumer creation regressed")
	}
	defer stopLegacy()
	if err := checkedStream(ctx, js); err != nil {
		t.Fatal("legacy stream specification changed")
	}
	t.Log("NATS_PROVISIONED_TLS_ACTUAL_PASS")
}

func itoaFixture(value int) string { b, _ := json.Marshal(value); return string(b) }

func natsFixtureTLS(t *testing.T) ([]byte, []byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal("fixture key failed")
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "isolated NATS fixture"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal("fixture CA failed")
	}
	ca, _ = x509.ParseCertificate(der)
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal("fixture leaf key failed")
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour)}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, key)
	if err != nil {
		t.Fatal("fixture leaf failed")
	}
	private, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		t.Fatal("fixture encoding failed")
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})
}
