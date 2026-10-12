package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"
)

func TestOrdinaryAccountRelayActualTLSNATSPG(t *testing.T) {
	binary, database := os.Getenv("PINVOU_TEST_NATS_SERVER_BINARY"), os.Getenv("PINVOU_TEST_EVENTS_DATABASE_URL")
	if binary == "" || database == "" {
		t.Skip("explicit owned NATS binary and PostgreSQL database are required")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("test server binary must be an explicit absolute path")
	}
	// Do not contact an inherited exporter. Production tracing rejection is
	// checked explicitly; the positive fixture is not an OTLP deployment test.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	ctx, finish := context.WithTimeout(context.Background(), 80*time.Second)
	defer finish()
	owned := t.TempDir()
	write := func(name string, data []byte) string {
		t.Helper()
		path := filepath.Join(owned, name)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal("controlled file write failed")
		}
		return path
	}
	operator, relay, observer := fixtureNKey(t), fixtureNKey(t), fixtureNKey(t)
	defer operator.Wipe()
	defer relay.Wipe()
	defer observer.Wipe()
	operatorPublic, _ := operator.PublicKey()
	relayPublic, _ := relay.PublicKey()
	observerPublic, _ := observer.PublicKey()
	operatorSeed, _ := operator.Seed()
	relaySeed, _ := relay.Seed()
	operatorPath, relayPath := write("operator.seed", operatorSeed), write("relay.seed", relaySeed)
	ca, cert, key := ordinaryTLS(t)
	caPath := write("ca.crt", ca)
	certPath := write("server.crt", cert)
	keyPath := write("server.key", key)
	config, err := os.ReadFile("../../deploy/production/nats.conf")
	if err != nil {
		t.Fatal("actual server template unavailable")
	}
	auth, err := os.ReadFile("../../deploy/production/authorization.conf")
	if err != nil {
		t.Fatal("actual authorization template unavailable")
	}
	// The additional test observer has only bounded stream read/update API
	// permissions for verification and drift injection; it is not deployed.
	auth = stringsToBytes(strings.Replace(string(auth), "users: [", fmt.Sprintf("users: [\n{nkey: %q, permissions: {publish: [\"$JS.API.STREAM.INFO.ACCOUNT_PLATFORM\",\"$JS.API.STREAM.MSG.GET.ACCOUNT_PLATFORM\",\"$JS.API.STREAM.UPDATE.ACCOUNT_PLATFORM\"], subscribe: [\"_INBOX.ordinary-test-observer.>\"]}},", observerPublic), 1))
	write("authorization.conf", auth)
	port := ordinaryFreePort(t)
	text := strings.NewReplacer(`"0.0.0.0:4222"`, fmt.Sprintf("\"127.0.0.1:%d\"", port), `http: "127.0.0.1:8222"`, "", `"/run/pinvou/nats-tls/server.crt"`, fmt.Sprintf("%q", filepath.ToSlash(certPath)), `"/run/pinvou/nats-tls/server.key"`, fmt.Sprintf("%q", filepath.ToSlash(keyPath)), `"/data"`, fmt.Sprintf("%q", filepath.ToSlash(filepath.Join(owned, "jetstream")))).Replace(string(config))
	configPath := write("nats.conf", []byte(text))
	server := exec.CommandContext(ctx, binary, "-c", configPath)
	server.Dir = owned
	server.Env = append(os.Environ(), "NATS_EVENT_RESOURCE_OPERATOR_PUBLIC_KEY="+operatorPublic, "NATS_ACCOUNT_OUTBOX_RELAY_PUBLIC_KEY="+relayPublic)
	server.Stdout = io.Discard
	server.Stderr = io.Discard
	if err := server.Start(); err != nil {
		t.Fatal("owned NATS process start failed")
	}
	done := make(chan error, 1)
	go func() { done <- server.Wait() }()
	defer func() {
		// Process is the exact exec handle started with this unique config/root.
		_ = server.Process.Kill()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("owned NATS process did not terminate")
		}
		if c, e := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond); e == nil {
			c.Close()
			t.Error("owned NATS listener remains after stop")
		}
	}()
	url := fmt.Sprintf("tls://127.0.0.1:%d", port)
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		t.Fatal("controlled CA invalid")
	}
	var connection *nats.Conn
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		connection, err = nats.Connect(url, nats.Secure(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}), nats.Nkey(observerPublic, observer.Sign), nats.CustomInboxPrefix("_INBOX.ordinary-test-observer"), nats.Timeout(time.Second), nats.ErrorHandler(func(*nats.Conn, *nats.Subscription, error) {}))
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatal("owned TLS NATS did not become available")
	}
	defer connection.Close()
	js, err := jetstream.New(connection)
	if err != nil {
		t.Fatal("controlled observer failed")
	}
	opConfig := events.NATSConfig{Environment: "production", ResourceMode: "provisioned", CAFile: caPath, SeedFile: operatorPath, RequireExplicitAuthentication: true}
	if err := events.ManageNATSResources(ctx, url, opConfig, false); err == nil {
		t.Fatal("missing stream passed read-only check")
	}
	relayConfig := opConfig
	relayConfig.SeedFile = relayPath
	if broker, err := events.NewNATSBroker(ctx, url, "account-outbox-relay", relayConfig); err == nil {
		broker.Close()
		t.Fatal("runtime created a missing stream")
	}
	if err := events.ManageNATSResources(ctx, url, opConfig, true); err != nil {
		t.Fatal("operator create-only failed")
	}
	if err := events.ManageNATSResources(ctx, url, opConfig, false); err != nil {
		t.Fatal("operator read-only verification failed")
	}
	stream, err := js.Stream(ctx, "ACCOUNT_PLATFORM")
	if err != nil {
		t.Fatal("fixed stream not readable")
	}
	original := stream.CachedInfo().Config
	drift := original
	drift.Description = "controlled drift"
	if _, err := js.UpdateStream(ctx, drift); err != nil {
		t.Fatal("controlled drift injection failed")
	}
	if err := events.ManageNATSResources(ctx, url, opConfig, true); err == nil {
		t.Fatal("create-only repaired existing drift")
	}
	if info, err := stream.Info(ctx); err != nil || info.Config.Description != "controlled drift" {
		t.Fatal("drift facts were overwritten")
	}
	if _, err := js.UpdateStream(ctx, original); err != nil {
		t.Fatal("test-only fixture restore failed")
	}
	wrongCA, _, _ := ordinaryTLS(t)
	wrongCAConfig := relayConfig
	wrongCAConfig.CAFile = write("wrong-ca.crt", wrongCA)
	if broker, err := events.NewNATSBroker(ctx, url, "account-outbox-relay", wrongCAConfig); err == nil {
		broker.Close()
		t.Fatal("runtime accepted an unrelated TLS CA")
	}
	broker, err := events.NewNATSBroker(ctx, url, "account-outbox-relay", relayConfig)
	if err != nil {
		t.Fatal("provisioned relay identity unavailable")
	}
	denied, cancelDenied := context.WithTimeout(ctx, 300*time.Millisecond)
	if err := broker.Publish(denied, "billing.controlled", []byte(`{}`), uuid.NewString()); err == nil {
		cancelDenied()
		broker.Close()
		t.Fatal("Account identity published a future-domain event")
	}
	cancelDenied()
	broker.Close()
	if info, err := stream.Info(ctx); err != nil || info.State.Msgs != 0 {
		t.Fatal("denied future-domain publish changed the fixed stream")
	}
	control, err := pgx.Connect(ctx, database)
	if err != nil {
		t.Fatal("task database connection failed")
	}
	defer control.Close(context.Background())
	schema := "relay_tls_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := control.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal("owned schema creation failed")
	}
	defer func() {
		c, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		if _, err := control.Exec(c, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error("owned schema cleanup failed")
		}
		var exists bool
		if err := control.QueryRow(c, `SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname=$1)`, schema).Scan(&exists); err != nil || exists {
			t.Error("owned schema absence unconfirmed")
		}
	}()
	if _, err := control.Exec(ctx, "SET search_path TO "+quoted); err != nil {
		t.Fatal("owned schema selection failed")
	}
	all, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal("installed outbox migrations unavailable")
	}
	var selected []migrations.Migration
	for _, m := range all {
		if m.Version == 1 || m.Version == 3 {
			selected = append(selected, m)
		}
	}
	if len(selected) != 2 || migrations.Up(ctx, control, selected) != nil {
		t.Fatal("actual ordinary outbox migration failed")
	}
	tx, err := control.Begin(ctx)
	if err != nil {
		t.Fatal("controlled transaction failed")
	}
	if err := events.AppendOutbox(ctx, tx, "account.user.updated", uuid.New(), 1, map[string]any{"controlled": true}); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal("actual outbox append failed")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal("actual outbox commit failed")
	}
	var eventID uuid.UUID
	if err := control.QueryRow(ctx, `SELECT event_id FROM outbox_events`).Scan(&eventID); err != nil {
		t.Fatal("actual event identity missing")
	}
	dbURL, err := neturl.Parse(database)
	if err != nil {
		t.Fatal("task database config invalid")
	}
	query := dbURL.Query()
	query.Set("search_path", schema)
	dbURL.RawQuery = query.Encode()
	httpPort := ordinaryFreePort(t)
	env := map[string]string{"EVENT_SOURCE": "account", "APP_ENV": "production", "DATABASE_URL": dbURL.String(), "NATS_URL": url, "NATS_CA_FILE": caPath, "NATS_NKEY_SEED_FILE": relayPath, "EVENT_RELAY_HTTP_ADDR": fmt.Sprintf("127.0.0.1:%d", httpPort)}
	if err := run(ctx, func(k string) string { return env[k] }, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil || err.Error() != "ACCOUNT_EVENT_TRACING_UNAVAILABLE" {
		t.Fatal("production missing tracing configuration did not fail closed")
	}
	// ResourceMode and explicit authentication remain provisioned in every
	// environment. Only tracing is omitted in this controlled component test.
	env["APP_ENV"] = "test"
	var activeCancel context.CancelFunc
	var activeResult <-chan error
	defer func() {
		if activeCancel != nil {
			activeCancel()
			select {
			case <-activeResult:
			case <-time.After(7 * time.Second):
				t.Error("owned relay did not stop after test failure")
			}
		}
	}()
	startRelay := func() (context.CancelFunc, <-chan error) {
		c, cancel := context.WithCancel(ctx)
		result := make(chan error, 1)
		go func() {
			result <- run(c, func(k string) string { return env[k] }, slog.New(slog.NewTextHandler(io.Discard, nil)))
		}()
		activeCancel, activeResult = cancel, result
		return cancel, result
	}
	stopRelay := func(cancel context.CancelFunc, result <-chan error) {
		t.Helper()
		cancel()
		select {
		case err := <-result:
			activeCancel, activeResult = nil, nil
			if err != nil {
				t.Fatal("ordinary relay shutdown failed")
			}
		case <-time.After(7 * time.Second):
			t.Fatal("ordinary relay shutdown timed out")
		}
	}
	waitPublished := func() {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
			select {
			case err := <-activeResult:
				activeCancel, activeResult = nil, nil
				t.Fatal("owned relay exited before publication:", err)
			default:
			}
			var published bool
			if err := control.QueryRow(ctx, `SELECT published_at IS NOT NULL FROM outbox_events WHERE event_id=$1`, eventID).Scan(&published); err == nil && published {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatal("real broker ACK did not commit SQL publication")
	}
	cancel, result := startRelay()
	waitPublished()
	response, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health/ready", httpPort))
	if err != nil {
		t.Fatal("actual ready endpoint failed")
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("actual ready endpoint not ready")
	}
	stopRelay(cancel, result)
	message, err := stream.GetMsg(ctx, 1)
	if err != nil || message.Header.Get(nats.MsgIdHdr) != eventID.String()+":0" {
		t.Fatal("actual NATS message ID lost generation")
	}
	if envelope, err := events.Unmarshal(message.Data); err != nil || envelope.ID != eventID {
		t.Fatal("actual published envelope mismatch")
	}
	// Controlled SQL resets only the acknowledgement fact to model an unknown
	// database outcome. There is no user-facing/manual replay API in F01.
	if _, err := control.Exec(ctx, `UPDATE outbox_events SET published_at=NULL,next_attempt_at=now() WHERE event_id=$1`, eventID); err != nil {
		t.Fatal("controlled unknown-ACK seed failed")
	}
	cancel, result = startRelay()
	waitPublished()
	stopRelay(cancel, result)
	if info, err := stream.Info(ctx); err != nil || info.State.Msgs != 1 {
		t.Fatal("same-generation retry duplicated broker delivery")
	}
	if _, err := control.Exec(ctx, `UPDATE outbox_events SET published_at=NULL,next_attempt_at=now(),publication_generation=publication_generation+1 WHERE event_id=$1`, eventID); err != nil {
		t.Fatal("controlled generation seed failed")
	}
	cancel, result = startRelay()
	waitPublished()
	stopRelay(cancel, result)
	if info, err := stream.Info(ctx); err != nil || info.State.Msgs != 2 {
		t.Fatal("new stored generation did not publish independently")
	}
	t.Log("ORDINARY_ACCOUNT_TLS_NATS_PG_ACK_GENERATION_PASS")
}

func fixtureNKey(t *testing.T) nkeys.KeyPair {
	t.Helper()
	pair, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal("controlled NKey generation failed")
	}
	return pair
}
func stringsToBytes(value string) []byte { return []byte(value) }
func ordinaryFreePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("owned loopback port failed")
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}
func ordinaryTLS(t *testing.T) ([]byte, []byte, []byte) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal("controlled CA generation failed")
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "owned ordinary relay fixture"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour)}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal("controlled CA signing failed")
	}
	ca, _ = x509.ParseCertificate(caDER)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal("controlled leaf generation failed")
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(2), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"}, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, cert, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal("controlled leaf signing failed")
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal("controlled key encoding failed")
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})
}
