package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"
)

func TestFreshApprovalConsumersActualTLSNATS(t *testing.T) {
	binary := os.Getenv("PINVOU_TEST_NATS_SERVER_BINARY")
	if binary == "" {
		t.Skip("explicit owned NATS binary is required")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("explicit owned binary required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
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
		pair := fixtureNKey(t)
		pairs[role] = pair
		defer pair.Wipe()
		keys[role], _ = pair.PublicKey()
	}
	oldAccount := fixtureNKey(t)
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
	ordinarySeed, _ := pairs["account-outbox-relay"].Seed()
	opPath := write(private, "operator.seed", operatorSeed)
	ordinaryPath := write(private, "ordinary.seed", ordinarySeed)
	identity := events.NATSAuthorizationArtifactFiles{CurrentFile: currentPath, CurrentSHA256: hex.EncodeToString(currentDigest[:]), PrivateIdentityFiles: []string{opPath}}
	manifest, configuration, err := identity.ConsumerAuthorizationArtifacts(time.Now().UTC())
	if err != nil {
		t.Fatal("actual bounded public renderer failed")
	}
	// The guarded identity directory contains only its identity artifacts;
	// independently trusted consumer proofs live in a separate directory.
	manifestPath := write(resources, "manifest.json", manifest)
	var auth map[string]any
	if json.Unmarshal(configuration.Configuration, &auth) != nil {
		t.Fatal("actual ACL decode failed")
	}
	observer := fixtureNKey(t)
	defer observer.Wipe()
	observerPublic, _ := observer.PublicKey()
	// Fixture-only observer creates the controlled forbidden legacy object.
	// It is neither in the generated identity nor an issued production role.
	users := auth["authorization"].(map[string]any)["users"].([]any)
	auth["authorization"].(map[string]any)["users"] = append(users, map[string]any{"nkey": observerPublic, "permissions": map[string]any{"publish": []string{"$JS.API.>", "account-platform.dead-letter.account.approval-command.authorized.v1"}, "subscribe": []string{"_INBOX.cutover-test.>"}}})
	ca, cert, key := ordinaryTLS(t)
	caPath := write(private, "ca.crt", ca)
	certPath := write(private, "server.crt", cert)
	keyPath := write(private, "server.key", key)
	port := ordinaryFreePort(t)
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
	ordinaryConfig := cfg
	ordinaryConfig.SeedFile = ordinaryPath
	broker, err := events.NewNATSBroker(ctx, url, "account-outbox-relay", ordinaryConfig)
	if err != nil {
		t.Fatal("ordinary broker unavailable")
	}
	defer broker.Close()
	if broker.Publish(ctx, "account.test.v1", []byte(`{"test":true}`), uuid.NewString()) != nil {
		t.Fatal("ordinary real publication failed")
	}
	receipt, err := events.InitializeFreshApprovalConsumers(ctx, url, cfg, identity, manifestPath, configuration.ManifestSHA256, events.FreshApprovalControl{Version: 3, ApprovalEpoch: 2, RelayEpoch: 2})
	if err != nil {
		t.Fatal("real Q capture and create-only initialization failed")
	}
	receiptPath := write(resources, "receipt.json", receipt)
	receiptDigest := sha256.Sum256(receipt)
	files := events.ApprovalConsumerResourceFiles{Identity: identity, ManifestFile: manifestPath, ManifestSHA256: configuration.ManifestSHA256, ReceiptFile: receiptPath, ReceiptSHA256: hex.EncodeToString(receiptDigest[:])}
	facts, err := events.CheckFreshApprovalConsumers(ctx, url, cfg, files)
	if err != nil || facts.Version != 3 || facts.ApprovalEpoch != 2 || facts.RelayEpoch != 2 {
		t.Fatal("real resources did not match independently bound receipt")
	}
	var wire struct {
		HighWatermark string `json:"high_watermark"`
		BillingStarts []struct {
			StartSequence string `json:"start_sequence"`
		} `json:"billing_starts"`
	}
	if json.Unmarshal(receipt, &wire) != nil || wire.HighWatermark != "1" || len(wire.BillingStarts) != 3 {
		t.Fatal("receipt lost actual high watermark or command scope")
	}
	for _, s := range wire.BillingStarts {
		if s.StartSequence != "2" {
			t.Fatal("receipt did not use actual Q+1")
		}
	}
	stream, err := js.Stream(ctx, "ACCOUNT_PLATFORM")
	if err != nil {
		t.Fatal("actual stream inspection failed")
	}
	t.Run("retained_authorization_deadletter_is_not_fresh", func(t *testing.T) {
		ack, err := js.Publish(ctx, "account-platform.dead-letter.account.approval-command.authorized.v1", []byte(`{"controlled":true}`), jetstream.WithMsgID(uuid.NewString()))
		if err != nil {
			t.Fatal("controlled retained DLQ publication failed")
		}
		if _, err = events.CheckFreshApprovalConsumers(ctx, url, cfg, files); err == nil {
			t.Fatal("retained authorization DLQ passed fresh readiness")
		}
		if _, err = events.InitializeFreshApprovalConsumers(ctx, url, cfg, identity, manifestPath, configuration.ManifestSHA256, events.FreshApprovalControl{Version: 3, ApprovalEpoch: 2, RelayEpoch: 2}); err == nil {
			t.Fatal("retained authorization DLQ passed fresh initialization")
		}
		if _, err = stream.GetMsg(ctx, ack.Sequence); err != nil {
			t.Fatal("rejected initializer removed retained evidence")
		}
		// Only this fixture's observer resets its controlled message so that
		// the independent legacy-consumer check below has its own cause.
		if err = stream.DeleteMsg(ctx, ack.Sequence); err != nil {
			t.Fatal("owned controlled message cleanup failed")
		}
	})
	if _, err = stream.CreateConsumer(ctx, jetstream.ConsumerConfig{Name: "billing-command-executor-v1", Durable: "billing-command-executor-v1", FilterSubject: "account.approval-command.authorized.v1", AckPolicy: jetstream.AckExplicitPolicy}); err != nil {
		t.Fatal("controlled legacy resource seed failed")
	}
	if _, err = events.CheckFreshApprovalConsumers(ctx, url, cfg, files); err == nil {
		t.Fatal("legacy consumer passed fresh readiness")
	}
	if _, err = events.InitializeFreshApprovalConsumers(ctx, url, cfg, identity, manifestPath, configuration.ManifestSHA256, events.FreshApprovalControl{Version: 3, ApprovalEpoch: 2, RelayEpoch: 2}); err == nil {
		t.Fatal("legacy consumer passed fresh installation")
	}
	if _, err = stream.Consumer(ctx, "billing-command-executor-v1"); err != nil {
		t.Fatal("blocked initializer deleted legacy evidence")
	}
}
