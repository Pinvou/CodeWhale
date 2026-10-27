package billingschema

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
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

type existingActualFixture struct {
	url       string
	cfg       events.NATSConfig
	files     events.ExistingBrokerResourceFiles
	js        jetstream.JetStream
	resources string
}

func existingPublicWrite(t *testing.T, directory, name string, raw []byte) string {
	t.Helper()
	p := filepath.Join(directory, name)
	if os.WriteFile(p, raw, 0600) != nil {
		t.Fatal("owned artifact write failed")
	}
	return p
}

func existingPublicHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func existingActualBroker(t *testing.T, ctx context.Context, binary string) existingActualFixture {
	t.Helper()
	root := t.TempDir()
	public, private, resources := filepath.Join(root, "identities"), filepath.Join(root, "private"), filepath.Join(root, "resources")
	for _, directory := range []string{public, private, resources} {
		if os.Mkdir(directory, 0700) != nil {
			t.Fatal("owned directory failed")
		}
	}
	roles := []string{"account-outbox-relay", "account-approval-relay", "billing-outbox-relay", "token-outbox-relay", "billing-command-executor", "token-command-executor", "event-resource-operator"}
	keys := map[string]string{}
	var operatorSeed []byte
	for _, role := range roles {
		pair := freshNKey(t)
		t.Cleanup(pair.Wipe)
		keys[role], _ = pair.PublicKey()
		if role == "event-resource-operator" {
			operatorSeed, _ = pair.Seed()
		}
	}
	previousKeys := map[string]string{}
	for role, key := range keys {
		if role != "account-approval-relay" {
			previousKeys[role] = key
		}
	}
	previous, _ := json.Marshal(map[string]any{"version": 1, "purpose": "pinvou-nats-service-identities", "public_keys": previousKeys})
	current, _ := json.Marshal(map[string]any{"version": 2, "purpose": "pinvou-nats-service-identities", "public_keys": keys, "previous_v1_sha256": existingPublicHash(previous), "overlap_expires_at": time.Now().UTC().Add(time.Hour).Truncate(time.Second).Format(time.RFC3339)})
	op := existingPublicWrite(t, private, "operator.seed", operatorSeed)
	identity := events.NATSAuthorizationArtifactFiles{CurrentFile: existingPublicWrite(t, public, "current.json", current), CurrentSHA256: existingPublicHash(current), PrivateIdentityFiles: []string{op}}
	manifest, rendered, err := identity.ExistingBrokerAuthorizationArtifacts(time.Now().UTC())
	if err != nil {
		t.Fatal("actual existing-broker ACL renderer failed")
	}
	var auth map[string]any
	if json.Unmarshal(rendered.Configuration, &auth) != nil {
		t.Fatal("ACL decode failed")
	}
	observer := freshNKey(t)
	t.Cleanup(observer.Wipe)
	observerPublic, _ := observer.PublicKey()
	users := auth["authorization"].(map[string]any)["users"].([]any)
	// This fixture-only identity seeds legacy resources and controlled drift.
	// It is not an issued production role or part of the trusted projection.
	auth["authorization"].(map[string]any)["users"] = append(users, map[string]any{"nkey": observerPublic, "permissions": map[string]any{"publish": []string{"$JS.API.>", "account.fixture.>", "account.approval-command.authorized.v1"}, "subscribe": []string{"_INBOX.existing-test.>"}}})
	ca, cert, key := freshTLS(t)
	caPath, certPath, keyPath := existingPublicWrite(t, private, "ca.crt", ca), existingPublicWrite(t, private, "server.crt", cert), existingPublicWrite(t, private, "server.key", key)
	port := freshFreePort(t)
	configuration := map[string]any{"listen": fmt.Sprintf("127.0.0.1:%d", port), "jetstream": map[string]any{"store_dir": filepath.Join(root, "data")}, "tls": map[string]any{"cert_file": certPath, "key_file": keyPath}, "authorization": auth["authorization"]}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(configuration) != nil {
		t.Fatal("server config encode failed")
	}
	configPath := existingPublicWrite(t, root, "nats.conf", encoded.Bytes())
	command := exec.CommandContext(ctx, binary, "-c", configPath)
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if command.Start() != nil {
		t.Fatal("owned NATS start failed")
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	t.Cleanup(func() {
		_ = command.Process.Kill()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("owned NATS cleanup unconfirmed")
		}
	})
	url := fmt.Sprintf("tls://127.0.0.1:%d", port)
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca)
	var connection *nats.Conn
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); {
		connection, err = nats.Connect(url, nats.Secure(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}), nats.Nkey(observerPublic, observer.Sign), nats.CustomInboxPrefix("_INBOX.existing-test"), nats.Timeout(time.Second), nats.ErrorHandler(func(*nats.Conn, *nats.Subscription, error) {}))
		if err == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err != nil {
		t.Fatal("owned TLS connection failed")
	}
	t.Cleanup(connection.Close)
	js, err := jetstream.New(connection)
	if err != nil {
		t.Fatal("owned observer API failed")
	}
	stream, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "ACCOUNT_PLATFORM", Description: "Pinvou account platform domain events", Subjects: []string{"account.>", "billing.>", "token.>", "account-platform.dead-letter.>"}, Storage: jetstream.FileStorage, Retention: jetstream.LimitsPolicy, MaxAge: 30 * 24 * time.Hour, Duplicates: 24 * time.Hour})
	if err != nil {
		t.Fatal("owned legacy stream failed")
	}
	for _, pair := range [][2]string{{"billing-command-executor-v1", "account.approval-command.authorized.v1"}, {"billing-refund-executor-v1", "billing.refund.requested.v1"}, {"token-model-lifecycle-executor-v1", "account.approval-command.authorized.v1"}, {"token-catalog-rollback-executor-v1", "account.approval-command.authorized.v1"}, {"token-credential-revoke-executor-v1", "account.approval-command.authorized.v1"}} {
		_, err = stream.CreateConsumer(ctx, jetstream.ConsumerConfig{Name: pair[0], Durable: pair[0], FilterSubject: pair[1], AckPolicy: jetstream.AckExplicitPolicy, AckWait: 30 * time.Second, MaxDeliver: -1, BackOff: []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute}})
		if err != nil {
			t.Fatal("owned published legacy consumer failed")
		}
	}
	if _, err := js.Publish(ctx, "account.fixture.ordinary.v1", []byte(`{"ordinary":true}`)); err != nil {
		t.Fatal("owned ordinary message failed")
	}
	files := events.ExistingBrokerResourceFiles{ApprovalConsumerResourceFiles: events.ApprovalConsumerResourceFiles{Identity: identity, ManifestFile: existingPublicWrite(t, resources, "manifest.json", manifest), ManifestSHA256: rendered.ManifestSHA256}}
	return existingActualFixture{url: url, cfg: events.NATSConfig{Environment: "production", ResourceMode: "provisioned", RequireExplicitAuthentication: true, CAFile: caPath, SeedFile: op}, files: files, js: js, resources: resources}
}

func TestExistingBrokerOperatorActualPGTLSNATS(t *testing.T) {
	raw, binary := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL"), os.Getenv("PINVOU_TEST_NATS_SERVER_BINARY")
	if raw == "" || binary == "" {
		t.Skip("explicit owned PostgreSQL and NATS fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, raw)
	all, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal("public migrations unavailable")
	}
	db := f.database(t, ctx, "account", "account_service")
	if accountschema.Up(ctx, db, all) != nil {
		t.Fatal("actual Account migrations failed")
	}
	dsn := f.serviceURL(t, db, "account_migrator")
	b := existingActualBroker(t, ctx, binary)
	for _, item := range []struct {
		step    account.FreshApprovalStep
		version int64
	}{{account.FreshApprovalQuiesce, 1}, {account.FreshApprovalFence, 2}} {
		if _, err := account.RunExistingBrokerApprovalStep(ctx, dsn, item.version, item.step, "", nil); err != nil {
			t.Fatal("actual empty-history fence failed")
		}
	}
	called := false
	if _, err := account.RunExistingBrokerApprovalStep(ctx, dsn, 3, account.ExistingBrokerUpgradeToken, string(bytes.Repeat([]byte{'0'}, 64)), func(context.Context, account.AdminApprovalControl) (json.RawMessage, error) {
		called = true
		return []byte(`{}`), nil
	}); err == nil || called {
		t.Fatal("uncommitted/unprepared plan reached broker callback")
	}
	var output []byte
	prepare := func(token bool) {
		step, name := account.ExistingBrokerPrepare, "existing-plan.json"
		if token {
			step, name = account.ExistingBrokerPrepareTokenUpgrade, "token-plan.json"
		}
		_, err := account.RunExistingBrokerApprovalStep(ctx, dsn, 3, step, "", func(ctx context.Context, control account.AdminApprovalControl) (json.RawMessage, error) {
			var e error
			output, e = events.PrepareExistingBrokerPlan(ctx, b.url, b.cfg, b.files.ApprovalConsumerResourceFiles, events.FreshApprovalControl{Version: control.Version, ApprovalEpoch: control.ApprovalEpoch, RelayEpoch: control.AuthorizationRelayEpoch}, uuid.NewString(), token)
			if e == nil {
				b.files.PlanFile = existingPublicWrite(t, b.resources, name, output)
				b.files.PlanSHA256 = existingPublicHash(output)
			}
			return output, e
		})
		if err != nil {
			t.Fatal("actual locked plan preparation failed")
		}
	}
	prepare(true)
	if _, err := account.RunExistingBrokerApprovalStep(ctx, dsn, 3, account.ExistingBrokerUpgradeToken, b.files.PlanSHA256, func(ctx context.Context, control account.AdminApprovalControl) (json.RawMessage, error) {
		return events.UpgradeLegacyTokenConsumers(ctx, b.url, b.cfg, b.files, events.FreshApprovalControl{Version: control.Version, ApprovalEpoch: control.ApprovalEpoch, RelayEpoch: control.AuthorizationRelayEpoch}, true, false)
	}); err != nil {
		t.Fatal("actual original-instance Token upgrade failed")
	}
	prepare(false)
	facts := events.FreshApprovalControl{Version: 3, ApprovalEpoch: 2, RelayEpoch: 2}
	if _, err := events.InitializeFreshApprovalConsumers(ctx, b.url, b.cfg, b.files.Identity, b.files.ManifestFile, b.files.ManifestSHA256, facts); err == nil {
		t.Fatal("original fresh initializer accepted legacy broker")
	}
	if _, err := account.RunExistingBrokerApprovalStep(ctx, dsn, 3, account.FreshApprovalResources, b.files.PlanSHA256, func(ctx context.Context, control account.AdminApprovalControl) (json.RawMessage, error) {
		var e error
		output, e = events.InitializeExistingBrokerConsumers(ctx, b.url, b.cfg, b.files, facts, true, false)
		if e == nil {
			b.files.ReceiptFile = existingPublicWrite(t, b.resources, "receipt.json", output)
			b.files.ReceiptSHA256 = existingPublicHash(output)
		}
		return output, e
	}); err != nil {
		t.Fatal("actual existing-broker initialization failed")
	}
	if binding, err := b.files.Load(time.Now().UTC()); err != nil || binding == nil {
		t.Fatal("real Billing reader rejected actual operator receipt")
	}
	if _, err := events.CheckCurrentApprovalConsumers(ctx, b.url, b.cfg, b.files.ApprovalConsumerResourceFiles); err == nil {
		t.Fatal("original fresh checker accepted existing origin")
	}
	for _, item := range []struct {
		step    account.FreshApprovalStep
		version int64
	}{{account.FreshApprovalActivate, 3}, {account.FreshApprovalResume, 4}} {
		if _, err := account.RunExistingBrokerApprovalStep(ctx, dsn, item.version, item.step, b.files.PlanSHA256, func(ctx context.Context, control account.AdminApprovalControl) (json.RawMessage, error) {
			got, e := events.CheckExistingBrokerConsumers(ctx, b.url, b.cfg, b.files)
			if e != nil || got != facts {
				return nil, events.ErrNATSResources
			}
			return b.files.VerifiedReceipt(time.Now().UTC())
		}); err != nil {
			t.Fatal("actual existing-broker activation or resume failed")
		}
	}
	stream, _ := b.js.Stream(ctx, "ACCOUNT_PLATFORM")
	info, _ := stream.Info(ctx)
	if info.State.Msgs != 1 || info.State.LastSeq != 1 || info.State.Consumers != 17 {
		t.Fatal("message preservation or exact new resource count failed")
	}
	var count int
	if db.QueryRow(ctx, `SELECT count(*) FROM public.audit_logs WHERE action='APPROVAL_EXISTING_BROKER_INITIALIZATION'`).Scan(&count) != nil || count != 8 {
		t.Fatal("distinct committed operator audit missing")
	}
}
