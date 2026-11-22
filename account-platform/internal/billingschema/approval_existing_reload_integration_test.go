package billingschema

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"
)

// The actual offline generator creates both bundles: v2 preserves the six v1
// identities and adds a different approval publisher. The real consumer then
// renders and reloads those exact bytes on one owned broker.
func TestExistingBrokerRetainedAccountIdentityReload(t *testing.T) {
	binary, identityBinary := os.Getenv("PINVOU_TEST_NATS_SERVER_BINARY"), os.Getenv("PINVOU_TEST_NATS_IDENTITY_BINARY")
	if binary == "" || identityBinary == "" {
		t.Skip("explicit owned TLS NATS and identity generator required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	root := t.TempDir()
	if os.Chmod(root, 0700) != nil {
		t.Fatal("owned custody parent mode failed")
	}
	for _, name := range []string{"identity", "private", "data"} {
		if os.Mkdir(filepath.Join(root, name), 0700) != nil {
			t.Fatal("owned directory failed")
		}
	}
	roles := []string{"account-outbox-relay", "account-approval-relay", "billing-outbox-relay", "token-outbox-relay", "billing-command-executor", "token-command-executor", "event-resource-operator"}
	// Public test recipient from the upstream age test fixtures; no real
	// recovery identity or user key is involved.
	recipient := "age1cy0su9fwf3gf9mw868g5yut09p6nytfmmnktexz2ya5uqg9vl9sss4euqm"
	recipientPath := existingPublicWrite(t, root, "recipient.txt", []byte(recipient))
	v1Dir, v2Dir := filepath.Join(root, "v1"), filepath.Join(root, "v2")
	generate := func(name string, configuration map[string]any) {
		t.Helper()
		raw, err := json.Marshal(configuration)
		if err != nil {
			t.Fatal("owned generator config invalid")
		}
		path := existingPublicWrite(t, root, name, raw)
		command := exec.CommandContext(ctx, identityBinary, "-config", path)
		command.Stdout, command.Stderr = io.Discard, io.Discard
		if command.Run() != nil {
			t.Fatal("actual owned identity generator failed")
		}
	}
	generate("v1.json", map[string]any{"recipient_file": recipientPath, "recipient_sha256": existingPublicHash([]byte(recipient)), "output_directory": v1Dir})
	generate("v2.json", map[string]any{"recipient_file": recipientPath, "recipient_sha256": existingPublicHash([]byte(recipient)), "output_directory": v2Dir, "artifact_version": 2, "previous_directory": v1Dir, "overlap_expires_at": time.Now().UTC().Add(time.Hour).Truncate(time.Second).Format(time.RFC3339), "direct_cutover_preserve_account_outbox": true})
	previous, err := os.ReadFile(filepath.Join(v1Dir, "public-keys.json"))
	if err != nil {
		t.Fatal("generated v1 public artifact unavailable")
	}
	current, err := os.ReadFile(filepath.Join(v2Dir, "public-keys.json"))
	if err != nil {
		t.Fatal("generated v2 public artifact unavailable")
	}
	var oldPublic, newPublic struct {
		Keys     map[string]string `json:"public_keys"`
		Previous string            `json:"previous_v1_sha256"`
	}
	if json.Unmarshal(previous, &oldPublic) != nil || json.Unmarshal(current, &newPublic) != nil || newPublic.Previous != existingPublicHash(previous) {
		t.Fatal("generated custody link invalid")
	}
	pairs, keys := map[string]nkeys.KeyPair{}, newPublic.Keys
	for _, role := range roles {
		seed, err := os.ReadFile(filepath.Join(v2Dir, role, "nkey.seed"))
		if err != nil {
			t.Fatal("generated fixture seed unavailable")
		}
		pair, err := nkeys.FromSeed(seed)
		clear(seed)
		if err != nil {
			t.Fatal("generated fixture seed invalid")
		}
		t.Cleanup(pair.Wipe)
		pairs[role] = pair
		public, err := pair.PublicKey()
		if err != nil || public != keys[role] {
			t.Fatal("generated fixture key binding invalid")
		}
		if role != "account-approval-relay" && oldPublic.Keys[role] != public {
			t.Fatal("generated direct cutover changed an existing identity")
		}
	}
	currentPath := existingPublicWrite(t, filepath.Join(root, "identity"), "current.json", current)
	identity := events.NATSAuthorizationArtifactFiles{CurrentFile: currentPath, CurrentSHA256: existingPublicHash(current)}
	if _, err := events.RenderNATSAuthorization(current, previous); err == nil {
		t.Fatal("retained Account key was accepted with a previous-v1 overlap")
	}
	v1, err := events.RenderNATSAuthorization(previous, nil)
	if err != nil {
		t.Fatal("owned v1 projection failed")
	}
	_, v2, err := identity.ExistingBrokerAuthorizationArtifacts(time.Now().UTC())
	if err != nil {
		t.Fatal("owned preserved-key v2 projection failed")
	}
	observer := freshNKey(t)
	t.Cleanup(observer.Wipe)
	observerPublic, _ := observer.PublicKey()
	ca, cert, key := freshTLS(t)
	certPath := existingPublicWrite(t, filepath.Join(root, "private"), "server.crt", cert)
	keyPath := existingPublicWrite(t, filepath.Join(root, "private"), "server.key", key)
	port := freshFreePort(t)
	configuration := func(projection []byte) []byte {
		t.Helper()
		var auth map[string]any
		if json.Unmarshal(projection, &auth) != nil {
			t.Fatal("owned projection decode failed")
		}
		users := auth["authorization"].(map[string]any)["users"].([]any)
		auth["authorization"].(map[string]any)["users"] = append(users, map[string]any{"nkey": observerPublic, "permissions": map[string]any{"publish": []string{"$JS.API.>"}, "subscribe": []string{"_INBOX.retained-reload-observer.>"}}})
		wire := map[string]any{"listen": fmt.Sprintf("127.0.0.1:%d", port), "jetstream": map[string]any{"store_dir": filepath.Join(root, "data")}, "tls": map[string]any{"cert_file": certPath, "key_file": keyPath}, "authorization": auth["authorization"]}
		var out bytes.Buffer
		encoder := json.NewEncoder(&out)
		encoder.SetEscapeHTML(false)
		if encoder.Encode(wire) != nil {
			t.Fatal("owned broker config encoding failed")
		}
		return out.Bytes()
	}
	configPath := existingPublicWrite(t, root, "nats.conf", configuration(v1))
	logPath := filepath.Join(root, "server.log")
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal("owned log unavailable")
	}
	server := exec.CommandContext(ctx, binary, "-c", configPath)
	server.Stdout, server.Stderr = logFile, logFile
	if server.Start() != nil {
		_ = logFile.Close()
		t.Fatal("owned server start failed")
	}
	done := make(chan error, 1)
	go func() { done <- server.Wait() }()
	t.Cleanup(func() {
		_ = server.Process.Kill()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("owned server cleanup unconfirmed")
		}
		_ = logFile.Close()
	})
	endpoint := fmt.Sprintf("tls://127.0.0.1:%d", port)
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca)
	connect := func(public string, pair nkeys.KeyPair, inbox string) *nats.Conn {
		t.Helper()
		var c *nats.Conn
		var err error
		for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); {
			c, err = nats.Connect(endpoint, nats.Secure(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}), nats.Nkey(public, pair.Sign), nats.CustomInboxPrefix(inbox), nats.Timeout(time.Second), nats.NoReconnect(), nats.ErrorHandler(func(*nats.Conn, *nats.Subscription, error) {}))
			if err == nil {
				t.Cleanup(c.Close)
				return c
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatal("owned TLS connection failed")
		return nil
	}
	observerConnection := connect(observerPublic, observer, "_INBOX.retained-reload-observer")
	observerJS, err := jetstream.New(observerConnection)
	if err != nil {
		t.Fatal("owned observer API failed")
	}
	stream, err := observerJS.CreateStream(ctx, jetstream.StreamConfig{Name: "ACCOUNT_PLATFORM", Description: "Pinvou account platform domain events", Subjects: []string{"account.>", "billing.>", "token.>", "account-platform.dead-letter.>"}, Storage: jetstream.FileStorage, Retention: jetstream.LimitsPolicy, MaxAge: 30 * 24 * time.Hour, Duplicates: 24 * time.Hour})
	if err != nil {
		t.Fatal("owned stream creation failed")
	}
	ordinary := connect(keys["account-outbox-relay"], pairs["account-outbox-relay"], "_INBOX.account-outbox-relay")
	ordinaryJS, err := jetstream.New(ordinary)
	if err != nil {
		t.Fatal("owned ordinary API failed")
	}
	subjects := []string{"account.approval-command.authorized.v1", "account-platform.dead-letter.account.approval-command.authorized.v1"}
	for _, subject := range subjects {
		ack, err := ordinaryJS.Publish(ctx, subject, []byte(`{"fixture":true}`))
		if err != nil {
			t.Fatal("v1 ordinary publication control failed")
		}
		if stream.DeleteMsg(ctx, ack.Sequence) != nil {
			t.Fatal("owned fixture cleanup before reload failed")
		}
	}
	if os.WriteFile(configPath, configuration(v2.Configuration), 0600) != nil {
		t.Fatal("owned reload configuration failed")
	}
	reload := exec.CommandContext(ctx, binary, "--signal", fmt.Sprintf("reload=%d", server.Process.Pid))
	reload.Stdout, reload.Stderr = io.Discard, io.Discard
	if reload.Run() != nil {
		t.Fatal("owned server reload signal failed")
	}
	for deadline := time.Now().Add(6 * time.Second); ; {
		body, _ := os.ReadFile(logPath)
		if strings.Contains(string(body), "Reloaded server configuration") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("owned server reload completion not observed")
		}
		time.Sleep(25 * time.Millisecond)
	}
	verifyDenied := func(client jetstream.JetStream) {
		t.Helper()
		for _, subject := range subjects {
			bounded, stop := context.WithTimeout(ctx, time.Second)
			_, err := client.Publish(bounded, subject, []byte(`{"fixture":true}`))
			stop()
			if err == nil {
				t.Fatal("retained ordinary identity can publish a sensitive subject")
			}
			info, err := stream.Info(ctx, jetstream.WithSubjectFilter(subject))
			if err != nil || info.State.Subjects[subject] != 0 {
				t.Fatal("retained ordinary identity added a sensitive message")
			}
		}
	}
	verifyDenied(ordinaryJS) // this connection existed before reload; reconnect is disabled
	freshOrdinary := connect(keys["account-outbox-relay"], pairs["account-outbox-relay"], "_INBOX.account-outbox-relay")
	freshOrdinaryJS, err := jetstream.New(freshOrdinary)
	if err != nil {
		t.Fatal("retained key did not reconnect")
	}
	if _, err := freshOrdinaryJS.Publish(ctx, "account.fixture.ordinary.v1", []byte(`{"fixture":true}`)); err != nil {
		t.Fatal("retained key lost ordinary publication")
	}
	verifyDenied(freshOrdinaryJS)
	wrong := freshNKey(t)
	t.Cleanup(wrong.Wipe)
	wrongPublic, _ := wrong.PublicKey()
	if unexpected, err := nats.Connect(endpoint, nats.Secure(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}), nats.Nkey(wrongPublic, wrong.Sign), nats.NoReconnect(), nats.Timeout(time.Second)); err == nil {
		unexpected.Close()
		t.Fatal("unissued identity authenticated")
	}
	approval := connect(keys["account-approval-relay"], pairs["account-approval-relay"], "_INBOX.account-approval-relay")
	approvalJS, err := jetstream.New(approval)
	if err != nil {
		t.Fatal("owned approval API failed")
	}
	for _, subject := range subjects {
		if _, err := approvalJS.Publish(ctx, subject, []byte(`{"fixture":true}`)); err != nil {
			t.Fatal("dedicated approval publisher lost an authorized subject")
		}
	}
	for _, item := range []struct {
		role, durable string
		billing       bool
	}{
		{"billing-command-executor", "billing-wallet-adjustment-executor-p1-v1", true},
		{"token-command-executor", "token-model-lifecycle-executor-v1", false},
	} {
		runtime := connect(keys[item.role], pairs[item.role], "_INBOX."+item.role)
		runtimeJS, err := jetstream.New(runtime)
		if err != nil {
			t.Fatal("owned runtime API failed")
		}
		want := jetstream.ConsumerConfig{Name: item.durable, Durable: item.durable, FilterSubject: subjects[0], AckPolicy: jetstream.AckExplicitPolicy, AckWait: 30 * time.Second, MaxDeliver: -1, MaxAckPending: 64}
		if item.billing {
			want.DeliverPolicy = jetstream.DeliverByStartSequencePolicy
			want.OptStartSeq = 4
		}
		bounded, stop := context.WithTimeout(ctx, time.Second)
		_, err = runtimeJS.CreateConsumer(bounded, "ACCOUNT_PLATFORM", want)
		stop()
		if err == nil {
			t.Fatal("runtime identity acquired consumer management")
		}
		if _, err := observerJS.CreateConsumer(ctx, "ACCOUNT_PLATFORM", want); err != nil {
			t.Fatal("fixture negative control had invalid create request")
		}
		if err := stream.DeleteConsumer(ctx, item.durable); err != nil {
			t.Fatal("owned negative-control cleanup failed")
		}
	}
}
