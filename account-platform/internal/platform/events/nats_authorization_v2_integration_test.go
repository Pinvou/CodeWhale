package events

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// This is an isolated TLS/NKey server test. Revocation uses a controlled
// restart, not a claim of hot reload or of production cutover completion.
func TestNATSAuthorizationV2ActualPermissionsAndRevocation(t *testing.T) {
	binary := os.Getenv("PINVOU_TEST_NATS_SERVER_BINARY")
	if binary == "" {
		t.Skip("explicit isolated NATS binary required")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("fixture binary must be absolute")
	}
	t.Setenv("NATS_CREDENTIALS_FILE", "")
	t.Setenv("NATS_NKEY_SEED_FILE", "")
	root := t.TempDir()
	if os.Chmod(root, 0700) != nil {
		t.Fatal("fixture private directory")
	}
	ca, cert, key := natsFixtureTLS(t)
	for name, body := range map[string][]byte{"ca.crt": ca, "server.crt": cert, "server.key": key} {
		if os.WriteFile(filepath.Join(root, name), body, 0600) != nil {
			t.Fatal("fixture TLS material")
		}
	}
	oldKeys, oldFiles := map[string]string{}, map[string]string{}
	for _, role := range fixedNATSRoleNames() {
		oldFiles[role], oldKeys[role] = natsFixtureSeed(t, root, "previous-"+role)
	}
	previous, err := json.Marshal(map[string]any{"version": 1, "purpose": "pinvou-nats-service-identities", "public_keys": oldKeys})
	if err != nil {
		t.Fatal("fixture previous artifact")
	}
	keys, files := map[string]string{}, map[string]string{}
	for role, public := range oldKeys {
		keys[role], files[role] = public, oldFiles[role]
	}
	for _, role := range []string{"account-outbox-relay", "account-approval-relay"} {
		files[role], keys[role] = natsFixtureSeed(t, root, "current-"+role)
	}
	digest := sha256.Sum256(previous)
	current, err := json.Marshal(map[string]any{"version": 2, "purpose": "pinvou-nats-service-identities", "public_keys": keys,
		"previous_v1_sha256": hex.EncodeToString(digest[:]), "overlap_expires_at": time.Now().UTC().Add(time.Hour).Truncate(time.Second).Format(time.RFC3339)})
	if err != nil {
		t.Fatal("fixture current artifact")
	}
	overlap, err := RenderNATSAuthorization(current, previous)
	if err != nil {
		t.Fatal("fixture overlap projection")
	}
	final, err := RenderNATSAuthorization(current, nil)
	if err != nil {
		t.Fatal("fixture final projection")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("fixture port")
	}
	address := listener.Addr().String()
	_ = listener.Close()
	configPath := filepath.Join(root, "server.json")
	var process *exec.Cmd
	stop := func() {
		if process != nil {
			_ = process.Process.Kill()
			_ = process.Wait()
			process = nil
		}
	}
	t.Cleanup(stop)
	start := func(policy []byte, users int) {
		t.Helper()
		var rendered struct {
			Authorization struct {
				Users []json.RawMessage `json:"users"`
			} `json:"authorization"`
		}
		if json.Unmarshal(policy, &rendered) != nil || len(rendered.Authorization.Users) != users {
			t.Fatal("fixture unique identity count")
		}
		config := map[string]any{"listen": address, "jetstream": map[string]any{"store_dir": filepath.Join(root, "data")},
			"tls":           map[string]any{"cert_file": filepath.Join(root, "server.crt"), "key_file": filepath.Join(root, "server.key"), "min_version": "1.2"},
			"authorization": rendered.Authorization}
		var body bytes.Buffer
		encoder := json.NewEncoder(&body)
		encoder.SetEscapeHTML(false)
		if encoder.Encode(config) != nil || os.WriteFile(configPath, body.Bytes(), 0600) != nil {
			t.Fatal("fixture server configuration rejected")
		}
		if err := exec.Command(binary, "-t", "-c", configPath).Run(); err != nil {
			t.Fatal("fixture server configuration validation failed")
		}
		process = exec.Command(binary, "-c", configPath)
		process.Stdout, process.Stderr = io.Discard, io.Discard
		if process.Start() != nil {
			process = nil
			t.Fatal("fixture server startup")
		}
	}
	connect := func(role, seedFile string) (*nats.Conn, jetstream.JetStream, error) {
		return connectNATS("tls://"+address, role, NATSConfig{Environment: "production-fixture", CAFile: filepath.Join(root, "ca.crt"), SeedFile: seedFile})
	}
	wait := func(condition func() bool) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if condition() {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatal("fixture readiness deadline")
	}
	start(overlap, 8)
	connections := map[string]*nats.Conn{}
	streams := map[string]jetstream.JetStream{}
	for _, role := range append(fixedNATSV2RoleNames(), "previous-account") {
		name, seedFile := role, files[role]
		if role == "previous-account" {
			name, seedFile = "account-outbox-relay", oldFiles["account-outbox-relay"]
		}
		wait(func() bool {
			connection, stream, err := connect(name, seedFile)
			if err != nil {
				return false
			}
			connections[role], streams[role] = connection, stream
			t.Cleanup(connection.Close)
			return true
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	publisher, err := NewNATSPublishOnlyBroker(ctx, "tls://"+address, "account-approval-relay", NATSConfig{
		Environment: "production-fixture", CAFile: filepath.Join(root, "ca.crt"),
		SeedFile: files["account-approval-relay"], RequireExplicitAuthentication: true,
	})
	if err != nil {
		t.Fatal("production publish-only constructor required stream inspection or provisioning")
	}
	t.Cleanup(publisher.Close)
	developmentPublisher, err := NewNATSPublishOnlyBroker(ctx, "tls://"+address, "account-approval-relay", NATSConfig{
		Environment: "development", CAFile: filepath.Join(root, "ca.crt"),
		SeedFile: files["account-approval-relay"], RequireExplicitAuthentication: true,
	})
	if err != nil {
		t.Fatal("nonproduction explicit-identity constructor failed")
	}
	t.Cleanup(developmentPublisher.Close)
	for _, consume := range []func(context.Context, string, string, ConsumerHandler) (func(), error){developmentPublisher.Consume, developmentPublisher.ConsumeUntilResolved} {
		stop, err := consume(ctx, "fixture", "account.approval-command.authorized.v1", func(context.Context, Event) error { return nil })
		if err == nil || stop != nil {
			t.Fatal("nonproduction publish-only broker accepted a consumer")
		}
	}
	if err := publisher.Ping(ctx); err != nil {
		t.Fatal("publish-only transport readiness failed before resource provisioning")
	}
	for _, consume := range []func(context.Context, string, string, ConsumerHandler) (func(), error){publisher.Consume, publisher.ConsumeUntilResolved} {
		stop, err := consume(ctx, "fixture", "account.approval-command.authorized.v1", func(context.Context, Event) error {
			t.Error("publish-only broker invoked consumer handler")
			return nil
		})
		if err == nil || stop != nil {
			t.Fatal("publish-only broker accepted a consumer")
		}
	}
	if ProvisionNATSResources(ctx, streams["event-resource-operator"], true) != nil {
		t.Fatal("restricted resource initialization")
	}
	authorizationSubject := "account.approval-command.authorized.v1"
	productionStream, err := streams["event-resource-operator"].Stream(ctx, platformStream)
	if err != nil {
		t.Fatal("inspect fixed resource duplicate window")
	}
	beforeDuplicate, err := productionStream.Info(ctx)
	if err != nil || beforeDuplicate.Config.Duplicates != 24*time.Hour {
		t.Fatal("production duplicate window is not 24 hours")
	}
	duplicateID, duplicateBody := uuid.NewString(), []byte(`{"dedup_fixture":true}`)
	firstAck, err := streams["account-approval-relay"].Publish(ctx, authorizationSubject, duplicateBody, jetstream.WithMsgID(duplicateID))
	if err != nil || firstAck.Duplicate {
		t.Fatal("first authorization message was not stored")
	}
	secondAck, err := streams["account-approval-relay"].Publish(ctx, authorizationSubject, duplicateBody, jetstream.WithMsgID(duplicateID))
	if err != nil || !secondAck.Duplicate || secondAck.Sequence != firstAck.Sequence {
		t.Fatal("same authorization bytes and Msg-Id were not deduplicated")
	}
	afterDuplicate, err := productionStream.Info(ctx)
	if err != nil || afterDuplicate.State.Msgs != beforeDuplicate.State.Msgs+1 {
		t.Fatal("duplicate authorization publication stored a second message")
	}
	for _, subject := range []string{authorizationSubject, "account-platform.dead-letter." + authorizationSubject} {
		if err := publisher.Publish(ctx, subject, []byte(`{"production_constructor":true}`), uuid.NewString()); err != nil {
			t.Fatal("production publish-only broker did not receive PubAck")
		}
		if err := developmentPublisher.Publish(ctx, subject, []byte(`{"development_constructor":true}`), uuid.NewString()); err != nil {
			t.Fatal("nonproduction explicit identity did not receive PubAck")
		}
	}
	for _, probe := range []struct{ role, subject string }{
		{"account-outbox-relay", "account.fixture.v1"}, {"account-approval-relay", authorizationSubject},
		{"account-approval-relay", "account-platform.dead-letter." + authorizationSubject}, {"previous-account", authorizationSubject},
	} {
		if _, err := streams[probe.role].Publish(ctx, probe.subject, []byte(`{"fixture":true}`), jetstream.WithMsgID(uuid.NewString())); err != nil {
			t.Fatalf("permitted publication rejected for %s", probe.role)
		}
	}
	for _, probe := range []struct{ role, subject string }{
		{"account-outbox-relay", authorizationSubject}, {"account-outbox-relay", "account-platform.dead-letter." + authorizationSubject},
		{"account-approval-relay", "account.fixture.v1"}, {"account-approval-relay", "billing.fixture.v1"},
		{"account-approval-relay", authorizationSubject + ".extra"}, {"account-approval-relay", "account-platform.dead-letter.account.other.v1"},
		{"account-approval-relay", "$JS.API.STREAM.INFO." + platformStream},
		{"event-resource-operator", authorizationSubject}, {"event-resource-operator", "account-platform.dead-letter." + authorizationSubject},
		{"billing-outbox-relay", authorizationSubject}, {"token-outbox-relay", authorizationSubject},
	} {
		denied := make(chan struct{}, 1)
		connection := connections[probe.role]
		connection.SetErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
			if strings.Contains(err.Error(), "Permissions Violation") {
				select {
				case denied <- struct{}{}:
				default:
				}
			}
		})
		if connection.Publish(probe.subject, []byte(`{}`)) != nil || connection.FlushTimeout(time.Second) != nil {
			t.Fatal("negative ACL probe transport failed")
		}
		select {
		case <-denied:
		case <-time.After(time.Second):
			t.Fatalf("server did not explicitly deny %s publication", probe.role)
		}
	}
	stop()
	start(final, 7)
	wait(func() bool {
		for role, connection := range connections {
			if role != "previous-account" && (!connection.IsConnected() || connection.FlushTimeout(time.Second) != nil) {
				return false
			}
		}
		return !connections["previous-account"].IsConnected() && publisher.Ping(ctx) == nil
	})
	if old, _, err := connect("account-outbox-relay", oldFiles["account-outbox-relay"]); err == nil {
		old.Close()
		t.Fatal("retired Account seed reconnected")
	}
	for _, role := range fixedNATSV2RoleNames() {
		if role == "account-approval-relay" {
			continue // Dedicated publisher intentionally has no stream metadata API.
		}
		if _, err := streams[role].Stream(ctx, platformStream); err != nil {
			t.Fatalf("retained identity lost stream access: %s", role)
		}
	}
	if _, err := streams["account-approval-relay"].Publish(ctx, authorizationSubject, []byte(`{"after":true}`), jetstream.WithMsgID(uuid.NewString())); err != nil {
		t.Fatal("dedicated authorization publication failed after revocation")
	}
	if err := publisher.Publish(ctx, authorizationSubject, []byte(`{"production_after":true}`), uuid.NewString()); err != nil {
		t.Fatal("production publish-only broker failed after revocation")
	}
	t.Log("NATS_AUTHORIZATION_V2_EXACT_ACL_AND_RESTART_REVOCATION_PASS")
	// The production ACL/24-hour test is complete. A separate, fixture-only
	// server policy and stream now exercise expiry in one second; this does not
	// claim that 24 hours elapsed, nor change the production stream specification.
	publisher.Close()
	developmentPublisher.Close()
	for _, connection := range connections {
		connection.Close()
	}
	stop()
	const shortStream, shortSubject = "GSA07_DEDUP_FIXTURE", "fixture.dedup.v1"
	var shortPolicy bytes.Buffer
	shortEncoder := json.NewEncoder(&shortPolicy)
	shortEncoder.SetEscapeHTML(false) // NATS config does not accept JSON Unicode escapes for >.
	err = shortEncoder.Encode(map[string]any{"authorization": map[string]any{"users": []any{map[string]any{
		"nkey": keys["event-resource-operator"], "permissions": map[string]any{
			"publish":   map[string]any{"allow": []string{"$JS.API.STREAM.CREATE." + shortStream, "$JS.API.STREAM.INFO." + shortStream, shortSubject}},
			"subscribe": map[string]any{"allow": []string{"_INBOX.event-resource-operator.>"}},
		},
	}}}})
	if err != nil {
		t.Fatal("encode isolated short-window policy")
	}
	start(shortPolicy.Bytes(), 1)
	var shortConnection *nats.Conn
	var shortJS jetstream.JetStream
	wait(func() bool {
		shortConnection, shortJS, err = connect("event-resource-operator", files["event-resource-operator"])
		return err == nil
	})
	t.Cleanup(shortConnection.Close)
	fixtureStream, err := shortJS.CreateStream(ctx, jetstream.StreamConfig{Name: shortStream, Subjects: []string{shortSubject}, Storage: jetstream.MemoryStorage, Duplicates: time.Second})
	if err != nil {
		t.Fatal("create isolated short-window stream")
	}
	shortID, shortBody := uuid.NewString(), []byte(`{"bounded_window_fixture":true}`)
	first, err := shortJS.Publish(ctx, shortSubject, shortBody, jetstream.WithMsgID(shortID))
	if err != nil || first.Duplicate {
		t.Fatal("short-window initial publish")
	}
	time.Sleep(1200 * time.Millisecond)
	second, err := shortJS.Publish(ctx, shortSubject, shortBody, jetstream.WithMsgID(shortID))
	if err != nil || second.Duplicate || second.Sequence <= first.Sequence {
		t.Fatal("expired duplicate window did not admit the same ID and bytes again")
	}
	info, err := fixtureStream.Info(ctx)
	if err != nil || info.State.Msgs != 2 {
		t.Fatal("expired-window duplicate was not observable as two stored messages")
	}
	t.Log("NATS_DEDUP_24H_CONFIG_AND_BOUNDED_EXPIRY_SEMANTICS_PASS")
}
