package billing

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
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func TestBillingLegacyApprovalConsumerCompatibilityIntegration(t *testing.T) {
	binary := os.Getenv("PINVOU_TEST_NATS_SERVER_BINARY")
	if binary == "" {
		t.Skip("explicit isolated NATS binary required")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("fixture requires an absolute NATS binary")
	}
	root := t.TempDir()
	ca, certificate, key := billingNATSFixtureTLS(t)
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
		"listen":    address,
		"jetstream": map[string]any{"store_dir": filepath.Join(root, "data")},
		"tls":       map[string]any{"cert_file": filepath.Join(root, "server.crt"), "key_file": filepath.Join(root, "server.key"), "min_version": "1.2"},
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
	var admin *nats.Conn
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		admin, err = nats.Connect("tls://"+address, nats.RootCAs(filepath.Join(root, "ca.crt")), nats.Timeout(time.Second))
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || admin == nil {
		t.Fatal("fixture readiness")
	}
	t.Cleanup(admin.Close)
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name: "ACCOUNT_PLATFORM", Description: "Pinvou account platform domain events",
		Subjects: []string{"account.>", "billing.>", "token.>", "account-platform.dead-letter.>"},
		Storage:  jetstream.FileStorage, Retention: jetstream.LimitsPolicy, MaxAge: 30 * 24 * time.Hour, Duplicates: 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	durable := approvalconsumer.LegacyBilling().Durable
	serverConsumer, err := js.CreateConsumer(ctx, "ACCOUNT_PLATFORM", jetstream.ConsumerConfig{
		Name: durable, Durable: durable, FilterSubject: approvalconsumer.AuthorizationSubject,
		AckPolicy: jetstream.AckExplicitPolicy, AckWait: 30 * time.Second, MaxDeliver: -1, MaxAckPending: approvalconsumer.MaxAckPending,
	})
	if err != nil {
		t.Fatal(err)
	}
	broker, err := events.NewNATSBroker(ctx, "tls://"+address, approvalconsumer.BillingWorkload, events.NATSConfig{
		Environment: "test", ResourceMode: "provisioned", CAFile: filepath.Join(root, "ca.crt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(broker.Close)

	consumer, account, domain, fixtureDelivery := authorizationConsumerFixture(t, "READY", 0)
	domain.attempt = authorizationDomainAttempt{Result: authorizationDomainResult{DomainResult: json.RawMessage(`{"ok":true}`)}}
	handled := make(chan struct{}, 2)
	stop, err := broker.ConsumeAuthorization(ctx, durable, func(ctx context.Context, delivery events.AuthorizationRawDelivery) {
		consumer.Handle(ctx, delivery)
		handled <- struct{}{}
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.Publish(ctx, approvalconsumer.AuthorizationSubject, fixtureDelivery.raw, jetstream.WithMsgID(fixtureDelivery.id)); err != nil {
		t.Fatal(err)
	}
	waitBillingAuthorizationDelivery(t, ctx, handled)
	stop()
	if account.terminal != 1 || domain.calls != 1 {
		t.Fatalf("legacy raw success did not complete state machine: terminal=%d domain=%d order=%v", account.terminal, domain.calls, account.order)
	}
	info, err := serverConsumer.Info(ctx)
	if err != nil || info.NumAckPending != 0 {
		t.Fatalf("legacy raw success was not DoubleAck confirmed: pending=%d err=%v", info.NumAckPending, err)
	}

	failedConsumer, failedAccount, failedDomain, _ := authorizationConsumerFixture(t, "READY", 0)
	failedAccount.errAt = "resolve"
	stopFailed, err := broker.ConsumeAuthorization(ctx, durable, func(ctx context.Context, delivery events.AuthorizationRawDelivery) {
		failedConsumer.Handle(ctx, delivery)
		handled <- struct{}{}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stopFailed)
	if _, err := js.Publish(ctx, approvalconsumer.AuthorizationSubject, []byte(`{"not":"an authorization event"}`), jetstream.WithMsgID(uuid.NewString())); err != nil {
		t.Fatal(err)
	}
	waitBillingAuthorizationDelivery(t, ctx, handled)
	stopFailed()
	info, err = serverConsumer.Info(ctx)
	if err != nil || info.NumAckPending != 1 || failedDomain.calls != 0 || failedAccount.terminal != 0 {
		t.Fatalf("legacy raw failure did not remain for NAK redelivery: pending=%d domain=%d terminal=%d err=%v", info.NumAckPending, failedDomain.calls, failedAccount.terminal, err)
	}
	streamInfo, err := stream.Info(ctx)
	if err != nil || streamInfo.State.Msgs != 2 {
		t.Fatalf("generic Term/DLQ changed the isolated stream: messages=%d err=%v", streamInfo.State.Msgs, err)
	}
}

func waitBillingAuthorizationDelivery(t *testing.T, ctx context.Context, handled <-chan struct{}) {
	t.Helper()
	select {
	case <-handled:
	case <-ctx.Done():
		t.Fatal("authorization delivery deadline")
	}
}

func billingNATSFixtureTLS(t *testing.T) ([]byte, []byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "isolated Billing NATS fixture"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour)}
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
	return pemBytes("CERTIFICATE", der), pemBytes("CERTIFICATE", leafDER), pemBytes("PRIVATE KEY", private)
}

func pemBytes(kind string, body []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: body})
}
