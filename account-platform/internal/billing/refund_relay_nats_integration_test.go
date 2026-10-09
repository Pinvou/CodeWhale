package billing

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"
	"io"
	"math/big"
	"net"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// Actual PostgreSQL outbox, TLS/NKey broker, retained consumer and journal.
// Payment/refund authority is controlled, not a live external supplier or E2E.
type refundRelayProvider struct {
	*retainedFinancialProvider
	order  string
	amount int64
	calls  atomic.Int64
}

func (p *refundRelayProvider) Refund(_ context.Context, order, number string, amount int64) (payment.Refund, error) {
	if order != p.order || amount != p.amount || number == "" {
		return payment.Refund{}, ErrConflict
	}
	p.calls.Add(1)
	return payment.Refund{MerchantRefundNo: number, ChannelRefundID: "controlled-" + number, AmountFen: amount, Status: "SUCCESS"}, nil
}
func TestRefundOutboxActualTLSNATSAndLedger(t *testing.T) {
	binary := os.Getenv("PINVOU_TEST_NATS_SERVER_BINARY")
	if binary == "" {
		t.Skip("explicit owned NATS binary required")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("owned absolute binary required")
	}
	r, ctx := retainedFinanceRepository(t)
	c, order, transaction, pack := retainedFinanceOrder(t, r, ctx, 1000000, true)
	provider := &refundRelayProvider{retainedFinancialProvider: c.provider.(*retainedFinancialProvider), order: order.MerchantOrderNo, amount: 100}
	c.provider = provider
	c.AllowNormalRefundCreation(true)
	stableKey := uuid.NewString()
	refund, created, err := c.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, stableKey, "controlled whole refund")
	if err != nil || !created || refund.Status != "REQUESTED" {
		t.Fatal("actual refund enqueue failed", err)
	}
	var event events.Event
	if err = r.pool.QueryRow(ctx, `SELECT event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at FROM outbox_events WHERE aggregate_id=$1 AND event_type='billing.refund.requested.v1' AND published_at IS NULL`, refund.ID).Scan(&event.ID, &event.Type, &event.AggregateID, &event.AggregateVersion, &event.Payload, &event.TraceID, &event.OccurredAt); err != nil {
		t.Fatal("actual queued event missing", err)
	}
	public, resources, private, owned := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	write := func(dir, name string, raw []byte) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if os.WriteFile(p, raw, 0600) != nil {
			t.Fatal("owned file write failed")
		}
		return p
	}
	roles := []string{"account-outbox-relay", "account-approval-relay", "billing-outbox-relay", "token-outbox-relay", "billing-command-executor", "token-command-executor", "event-resource-operator"}
	keys := map[string]string{}
	pairs := map[string]nkeys.KeyPair{}
	for _, role := range roles {
		pair := refundNKey(t)
		pairs[role] = pair
		defer pair.Wipe()
		keys[role], _ = pair.PublicKey()
	}
	oldAccount := refundNKey(t)
	defer oldAccount.Wipe()
	old := map[string]string{}
	for _, role := range roles {
		if role != "account-approval-relay" {
			old[role] = keys[role]
		}
	}
	old["account-outbox-relay"], _ = oldAccount.PublicKey()
	previous, _ := json.Marshal(map[string]any{"version": 1, "purpose": "pinvou-nats-service-identities", "public_keys": old})
	digest := sha256.Sum256(previous)
	current, _ := json.Marshal(map[string]any{"version": 2, "purpose": "pinvou-nats-service-identities", "public_keys": keys, "previous_v1_sha256": hex.EncodeToString(digest[:]), "overlap_expires_at": time.Now().UTC().Add(time.Hour).Truncate(time.Second).Format(time.RFC3339)})
	currentPath := write(public, "current.json", current)
	currentDigest := sha256.Sum256(current)
	operatorSeed, _ := pairs["event-resource-operator"].Seed()
	ordinarySeed, _ := pairs["billing-outbox-relay"].Seed()
	opPath := write(private, "operator.seed", operatorSeed)
	ordinaryPath := write(private, "ordinary.seed", ordinarySeed)
	identity := events.NATSAuthorizationArtifactFiles{CurrentFile: currentPath, CurrentSHA256: hex.EncodeToString(currentDigest[:]), PrivateIdentityFiles: []string{opPath}}
	_, configuration, err := identity.ConsumerAuthorizationArtifacts(time.Now().UTC())
	if err != nil {
		t.Fatal("actual bounded public renderer failed")
	}
	// The guarded identity directory contains only its identity artifacts;
	// independently trusted consumer proofs live in a separate directory.
	_ = resources
	var auth map[string]any
	if json.Unmarshal(configuration.Configuration, &auth) != nil {
		t.Fatal("actual ACL decode failed")
	}
	observer := refundNKey(t)
	defer observer.Wipe()
	observerPublic, _ := observer.PublicKey()
	// Fixture-only observer creates the controlled forbidden legacy object.
	// It is neither in the generated identity nor an issued production role.
	users := auth["authorization"].(map[string]any)["users"].([]any)
	auth["authorization"].(map[string]any)["users"] = append(users, map[string]any{"nkey": observerPublic, "permissions": map[string]any{"publish": []string{"$JS.API.>"}, "subscribe": []string{"_INBOX.cutover-test.>"}}})
	ca, cert, key := refundTLS(t)
	caPath := write(private, "ca.crt", ca)
	certPath := write(private, "server.crt", cert)
	keyPath := write(private, "server.key", key)
	port := refundFreePort(t)
	config := map[string]any{"listen": fmt.Sprintf("127.0.0.1:%d", port), "jetstream": map[string]any{"store_dir": filepath.Join(owned, "data")}, "tls": map[string]any{"cert_file": certPath, "key_file": keyPath}, "authorization": auth["authorization"]}
	var configRaw bytes.Buffer
	encoder := json.NewEncoder(&configRaw)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(config) != nil {
		t.Fatal("controlled config encoding failed")
	}
	configPath := write(owned, "nats.conf", configRaw.Bytes())
	check := exec.CommandContext(ctx, binary, "-t", "-c", configPath)
	check.Stdout = io.Discard
	check.Stderr = io.Discard
	if check.Run() != nil {
		t.Fatal("controlled config syntax failed")
	}
	server := exec.CommandContext(ctx, binary, "-c", configPath)
	server.Dir = owned
	server.Stdout = io.Discard
	server.Stderr = io.Discard
	if server.Start() != nil {
		t.Fatal("owned NATS start failed")
	}
	done := make(chan error, 1)
	go func() { done <- server.Wait() }()
	defer func() {
		_ = server.Process.Kill()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("owned process stop unconfirmed")
		}
		if c, e := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond); e == nil {
			c.Close()
			t.Error("owned listener remains")
		}
	}()
	url := fmt.Sprintf("tls://127.0.0.1:%d", port)
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca)
	var observerConnection *nats.Conn
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); {
		observerConnection, err = nats.Connect(url, nats.Secure(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}), nats.Nkey(observerPublic, observer.Sign), nats.CustomInboxPrefix("_INBOX.cutover-test"), nats.Timeout(time.Second), nats.ErrorHandler(func(*nats.Conn, *nats.Subscription, error) {}))
		if err == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err != nil {
		t.Fatal("owned TLS server unavailable")
	}
	defer observerConnection.Close()
	js, err := jetstream.New(observerConnection)
	if err != nil {
		t.Fatal("observer API failed")
	}
	cfg := events.NATSConfig{Environment: "production", ResourceMode: "provisioned", RequireExplicitAuthentication: true, CAFile: caPath, SeedFile: opPath}
	if events.ManageNATSResources(ctx, url, cfg, true) != nil {
		t.Fatal("real fixed stream create failed")
	}

	// This real operator connection has only the generated resource API ACL;
	// observer authority is fixture-only and used to read the actual ACK state.
	operatorConnection, err := nats.Connect(url, nats.Secure(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}), nats.Nkey(keys["event-resource-operator"], pairs["event-resource-operator"].Sign), nats.CustomInboxPrefix("_INBOX.event-resource-operator"), nats.Timeout(time.Second), nats.ErrorHandler(func(*nats.Conn, *nats.Subscription, error) {}))
	if err != nil {
		t.Fatal("actual operator connection failed")
	}
	defer operatorConnection.Close()
	operatorJS, err := jetstream.New(operatorConnection)
	if err != nil {
		t.Fatal(err)
	}
	if err = events.ProvisionNATSResources(ctx, operatorJS, true); err != nil {
		t.Fatal("actual refund resource provisioning failed")
	}
	publishCfg := cfg
	publishCfg.SeedFile = ordinaryPath
	publisher, err := events.NewNATSBroker(ctx, url, "billing-outbox-relay", publishCfg)
	if err != nil {
		t.Fatal("billing publisher failed", err)
	}
	defer publisher.Close()
	executorSeed, _ := pairs["billing-command-executor"].Seed()
	executorCfg := cfg
	executorCfg.SeedFile = write(private, "executor.seed", executorSeed)
	executor, err := events.NewNATSBroker(ctx, url, "billing-command-executor", executorCfg)
	if err != nil {
		t.Fatal("actual executor connection failed", err)
	}
	defer executor.Close()
	consumer := NewRefundCommandConsumer(c)
	stop, err := executor.ConsumeUntilResolved(ctx, "billing-refund-executor-v1", "billing.refund.requested.v1", consumer.Handle)
	if err != nil {
		t.Fatal("actual retained refund consumer rejected current provisioned resource", err)
	}
	defer stop()
	// pgx retains the original parsed ConnString when fields are then changed;
	// derive the connection to this fixture's actual owned database explicitly.
	storeURL, err := neturl.Parse(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if err != nil {
		t.Fatal("fixture outbox URL parse failed")
	}
	storeURL.Path = "/" + r.pool.Config().ConnConfig.Database
	storeURL.User = neturl.User(r.pool.Config().ConnConfig.User)
	store, err := events.NewPostgresStore(ctx, storeURL.String())
	if err != nil {
		t.Fatal("billing outbox connection failed", err)
	}
	defer store.Close()
	relay := events.Relay{Store: store, Broker: publisher, WorkerID: uuid.NewString(), BatchSize: 100, AllowedSubjects: []string{"billing.>"}}
	result, err := relay.RunOnce(ctx)
	if err != nil || result.Published < 1 || result.Failed != 0 {
		t.Fatal("actual billing publication/PG ACK failed", result, err)
	}
	observed, err := js.Consumer(ctx, "ACCOUNT_PLATFORM", "billing-refund-executor-v1")
	if err != nil {
		t.Fatal("actual consumer inspection failed")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var status string
		if err = r.pool.QueryRow(ctx, `SELECT status FROM refunds WHERE id=$1`, refund.ID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		info, e := observed.Info(ctx)
		if status == "SUCCEEDED" && e == nil && info.AckFloor.Stream > 0 && info.NumAckPending == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("actual refund/JetStream ACK did not converge")
		}
		time.Sleep(20 * time.Millisecond)
	}
	assertFacts := func() {
		t.Helper()
		var posted, reserved, journals, net, attempts, generation int64
		var status string
		var published bool
		if err := r.pool.QueryRow(ctx, `SELECT posted_microcredits,reserved_microcredits FROM wallets WHERE id=$1`, order.WalletID).Scan(&posted, &reserved); err != nil || posted != 0 || reserved != 0 {
			t.Fatal("refund wallet effect not exact", posted, reserved, err)
		}
		if err := r.pool.QueryRow(ctx, `SELECT status FROM entitlement_packages WHERE id=$1`, pack).Scan(&status); err != nil || status != "REVOKED" {
			t.Fatal("whole package not revoked", status, err)
		}
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM journal_entries WHERE entry_type='REFUND'`).Scan(&journals); err != nil || journals != 1 {
			t.Fatal("refund journal not unique", journals, err)
		}
		if err := r.pool.QueryRow(ctx, `SELECT COALESCE(sum(delta_microcredits),0) FROM ledger_postings WHERE journal_entry_id IN (SELECT id FROM journal_entries WHERE entry_type='REFUND')`).Scan(&net); err != nil || net != 0 {
			t.Fatal("refund postings not balanced", net, err)
		}
		if err := r.pool.QueryRow(ctx, `SELECT published_at IS NOT NULL,attempts,publication_generation FROM outbox_events WHERE event_id=$1`, event.ID).Scan(&published, &attempts, &generation); err != nil || !published || attempts != 1 || generation != 0 {
			t.Fatal("outbox acknowledgement/generation not exact", err)
		}
	}
	assertFacts()
	if err = consumer.Handle(ctx, event); err != nil {
		t.Fatal("same command redelivery failed", err)
	}
	if same, replayed, e := c.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, stableKey, "controlled whole refund"); e != nil || replayed || same.ID != refund.ID {
		t.Fatal("original logical receipt not recovered", e)
	}
	assertFacts()
	if provider.calls.Load() != 1 {
		t.Fatal("replay dispatched provider refund again")
	}
	t.Log("ACTUAL_BILLING_REFUND_TLS_NATS_PG_LEDGER_ACK_REPLAY_PASS")
}
func refundNKey(t *testing.T) nkeys.KeyPair {
	t.Helper()
	pair, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal("controlled NKey generation failed")
	}
	return pair
}

func refundFreePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("owned loopback port failed")
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}
func refundTLS(t *testing.T) ([]byte, []byte, []byte) {
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
