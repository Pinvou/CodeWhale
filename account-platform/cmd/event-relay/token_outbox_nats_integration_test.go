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
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func TestOrdinaryTokenRelayActualTLSNATSPG(t *testing.T) {
	binary, raw := os.Getenv("PINVOU_TEST_NATS_SERVER_BINARY"), os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if binary == "" || raw == "" {
		t.Skip("explicit owned PostgreSQL and NATS binary are required")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("explicit absolute NATS binary required")
	}
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	f := newTokenRelayFixture(t, raw)
	owned := t.TempDir()
	for _, directory := range []string{"public", "private", "server"} {
		if err := os.Mkdir(filepath.Join(owned, directory), 0700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(directory, name string, body []byte) string {
		t.Helper()
		path := filepath.Join(owned, directory, name)
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	keys, seeds := map[string]string{}, map[string]string{}
	for _, role := range []string{"account-outbox-relay", "account-approval-relay", "billing-outbox-relay", "token-outbox-relay", "billing-command-executor", "token-command-executor", "event-resource-operator"} {
		pair := fixtureNKey(t)
		t.Cleanup(pair.Wipe)
		keys[role], _ = pair.PublicKey()
		seed, _ := pair.Seed()
		seeds[role] = write("private", role+".seed", seed)
		clear(seed)
	}
	current, err := json.Marshal(map[string]any{"version": 2, "purpose": "pinvou-nats-service-identities", "public_keys": keys,
		"previous_v1_sha256": strings.Repeat("0", 64), "overlap_expires_at": time.Now().UTC().Add(time.Hour).Truncate(time.Second).Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(current)
	identityPath := write("public", "current.json", current)
	identity := events.NATSAuthorizationArtifactFiles{CurrentFile: identityPath, CurrentSHA256: hex.EncodeToString(digest[:]), PrivateIdentityFiles: []string{seeds["token-outbox-relay"], seeds["event-resource-operator"]}}
	_, rendered, err := identity.TokenOutboxAuthorizationArtifacts(time.Now().UTC(), events.TokenOutboxAuthorizationFresh)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture observer can inspect this owned stream, but cannot publish
	// domain messages. It is additional test authority, never deployed output.
	observer := fixtureNKey(t)
	defer observer.Wipe()
	observerPublic, _ := observer.PublicKey()
	var authorization struct {
		Authorization struct {
			Users []any `json:"users"`
		} `json:"authorization"`
	}
	if err = json.Unmarshal(rendered.Configuration, &authorization); err != nil {
		t.Fatal(err)
	}
	authorization.Authorization.Users = append(authorization.Authorization.Users, map[string]any{"nkey": observerPublic,
		"permissions": map[string]any{"publish": []string{"$JS.API.STREAM.INFO.ACCOUNT_PLATFORM", "$JS.API.STREAM.MSG.GET.ACCOUNT_PLATFORM"}, "subscribe": []string{"_INBOX.token-outbox-observer.>"}}})
	var auth bytes.Buffer
	encoder := json.NewEncoder(&auth)
	encoder.SetEscapeHTML(false)
	if err = encoder.Encode(authorization); err != nil {
		t.Fatal(err)
	}
	write("server", "authorization.conf", auth.Bytes())
	ca, certificate, key := ordinaryTLS(t)
	caPath := write("server", "ca.crt", ca)
	certificatePath := write("server", "server.crt", certificate)
	keyPath := write("server", "server.key", key)
	configuration, err := os.ReadFile("../../deploy/production/nats.conf")
	if err != nil {
		t.Fatal(err)
	}
	port := ordinaryFreePort(t)
	serverConfig := strings.NewReplacer(`"0.0.0.0:4222"`, fmt.Sprintf("%q", fmt.Sprintf("127.0.0.1:%d", port)),
		`http: "127.0.0.1:8222"`, "", `"/run/pinvou/nats-tls/server.crt"`, fmt.Sprintf("%q", filepath.ToSlash(certificatePath)),
		`"/run/pinvou/nats-tls/server.key"`, fmt.Sprintf("%q", filepath.ToSlash(keyPath)), `"/data"`, fmt.Sprintf("%q", filepath.ToSlash(filepath.Join(owned, "server", "data")))).Replace(string(configuration))
	configPath := write("server", "nats.conf", []byte(serverConfig))
	server := exec.CommandContext(f.ctx, binary, "-c", configPath)
	server.Dir, server.Stdout, server.Stderr = filepath.Join(owned, "server"), io.Discard, io.Discard
	if err = server.Start(); err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Wait() }()
	t.Cleanup(func() {
		_ = server.Process.Kill()
		select {
		case <-serverDone:
		case <-time.After(5 * time.Second):
			t.Error("owned NATS process did not terminate")
		}
		if connection, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond); err == nil {
			connection.Close()
			t.Error("owned NATS listener remains after stop")
		}
	})
	brokerURL := fmt.Sprintf("tls://127.0.0.1:%d", port)
	wait := func(t *testing.T, label string, check func() bool) {
		t.Helper()
		deadline := time.NewTimer(8 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			if check() {
				return
			}
			select {
			case <-tick.C:
			case <-deadline.C:
				t.Fatal("actual Token relay condition missing", label)
			case <-f.ctx.Done():
				t.Fatal(f.ctx.Err())
			}
		}
	}
	operatorConfig := events.NATSConfig{Environment: "production", ResourceMode: "provisioned", CAFile: caPath, SeedFile: seeds["event-resource-operator"], RequireExplicitAuthentication: true}
	wait(t, "fixed stream provisioned", func() bool { return events.ManageNATSResources(f.ctx, brokerURL, operatorConfig, true) == nil })
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		t.Fatal("fixture CA")
	}
	observerConnection, err := nats.Connect(brokerURL, nats.Secure(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}),
		nats.Name("token-outbox-observer"), nats.CustomInboxPrefix("_INBOX.token-outbox-observer"),
		nats.Nkey(observerPublic, func(nonce []byte) ([]byte, error) { return observer.Sign(nonce) }))
	if err != nil {
		t.Fatal(err)
	}
	defer observerConnection.Close()
	js, err := jetstream.New(observerConnection)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.Stream(f.ctx, "ACCOUNT_PLATFORM")
	if err != nil {
		t.Fatal(err)
	}
	modelID, resourceID, userID, walletID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if _, err = f.pool.Exec(f.ctx, `INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at)
 VALUES($1,'controlled-relay','v1','controlled-model',$2,$3,now())`, modelID, strings.Repeat("a", 64), userID); err != nil {
		t.Fatal(err)
	}
	// Retained legal repair state is synthetic; the original domain sync writes
	// the case and its real envelope. No provider or financial executor runs.
	if _, err = f.pool.Exec(f.ctx, `INSERT INTO route_provisionings(id,scope_type,scope_id,wallet_id,model_id,upstream_model_ref,protocol_code,protocol_version_id,
 desired_generation,desired_signature,status,stage,policy_version,policy_snapshot,attempt_count,max_attempts,sla_deadline_at,requested_by,created_at,updated_at)
 VALUES($1,'PERSONAL',$2,$3,$4,'controlled-model','CHAT','00000000-0000-7000-8200-000000000000',1,$5,'REPAIR_REQUIRED','REVIEW',1,'{}',8,8,now()-interval '1 minute',$2,now(),now())`, resourceID, userID, walletID, modelID, strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	if count, err := f.repository.SyncManualReviewCases(f.ctx, 100); err != nil || count != 1 {
		t.Fatal("original Token domain writer", count, err)
	}
	var eventID uuid.UUID
	var payload string
	if err = f.pool.QueryRow(f.ctx, `SELECT event_id,jsonb_build_object('event_id',event_id,'event_type',event_type,
 'aggregate_id',aggregate_id,'aggregate_version',aggregate_version,'occurred_at',to_char(occurred_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
 'trace_id',trace_id,'payload',payload)::text FROM outbox_events WHERE event_type='token.manual-review.created.v1'`).Scan(&eventID, &payload); err != nil {
		t.Fatal(err)
	}
	immutable := func() string {
		t.Helper()
		var value string
		if err := f.pool.QueryRow(f.ctx, `SELECT jsonb_build_array(
 (SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM route_provisionings t),
 (SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM manual_review_cases t),
 (SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM manual_review_case_events t))::text`).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := immutable()
	environment := map[string]string{"APP_ENV": "development", "EVENT_SOURCE": "token", "DATABASE_URL": f.runtimeURL,
		"NATS_URL": brokerURL, "NATS_CA_FILE": caPath, "NATS_NKEY_SEED_FILE": seeds["token-outbox-relay"],
		"TOKEN_RELAY_NATS_CURRENT_PUBLIC_KEYS_FILE": identityPath, "TOKEN_RELAY_NATS_CURRENT_PUBLIC_KEYS_SHA256": identity.CurrentSHA256,
		"EVENT_RELAY_HTTP_ADDR": fmt.Sprintf("127.0.0.1:%d", ordinaryFreePort(t))}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Run("publisher-scope", func(t *testing.T) {
		broker, err := events.NewNATSBroker(f.ctx, brokerURL, "token-outbox-relay", events.NATSConfig{Environment: "production", ResourceMode: "provisioned", CAFile: caPath,
			SeedFile: seeds["token-outbox-relay"], ExpectedPublicKey: keys["token-outbox-relay"], RequireExplicitAuthentication: true})
		if err != nil {
			t.Fatal(err)
		}
		defer broker.Close()
		if err := broker.Publish(f.ctx, "token.controlled-probe.v1", []byte(`{}`), uuid.NewString()); err != nil {
			t.Fatal("Token publisher positive control", err)
		}
		for _, subject := range []string{"account.controlled-probe.v1", "billing.controlled-probe.v1", "account.approval-command.authorized.v1"} {
			denied, stop := context.WithTimeout(f.ctx, 200*time.Millisecond)
			err := broker.Publish(denied, subject, []byte(`{}`), uuid.NewString())
			stop()
			if err == nil {
				t.Fatal("Token publisher acquired another domain capability")
			}
		}
		info, err := stream.Info(f.ctx)
		if err != nil || info.State.Msgs != 1 {
			t.Fatal("denied capabilities changed stream", err)
		}
	})
	t.Run("wrong-initial-identity", func(t *testing.T) {
		wrong := func(key string) string {
			if key == "NATS_NKEY_SEED_FILE" {
				return seeds["account-outbox-relay"]
			}
			return environment[key]
		}
		if err := run(f.ctx, wrong, logger); err == nil || err.Error() != "ACCOUNT_EVENT_BROKER_UNAVAILABLE" {
			t.Fatal("wrong initial publisher identity was accepted", err)
		}
		var attempts int
		if err := f.pool.QueryRow(f.ctx, `SELECT attempts FROM outbox_events WHERE event_id=$1`, eventID).Scan(&attempts); err != nil || attempts != 0 {
			t.Fatal("wrong identity claimed the real domain envelope", attempts, err)
		}
	})
	t.Run("wrong-database-owner", func(t *testing.T) {
		name := f.newDB()
		config, err := pgx.ParseConfig(raw)
		if err != nil {
			t.Fatal(err)
		}
		config.Database = name
		admin, err := pgx.ConnectConfig(f.ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		defer admin.Close(context.Background())
		if _, err = admin.Exec(f.ctx, `CREATE TABLE public.outbox_events(publication_generation bigint NOT NULL DEFAULT 0)`); err != nil {
			admin.Close(context.Background())
			t.Fatal(err)
		}
		wrongURL, err := url.Parse(f.runtimeURL)
		if err != nil {
			t.Fatal(err)
		}
		wrongURL.Path = "/" + name
		if store, err := events.NewOwnedPostgresStore(f.ctx, wrongURL.String(), "token_schema_owner"); err == nil {
			store.Close()
			t.Fatal("wrong-owner outbox was accepted")
		}
		if _, err := admin.Exec(f.ctx, `ALTER TABLE public.outbox_events OWNER TO token_schema_owner; ALTER TABLE public.outbox_events DROP COLUMN publication_generation`); err != nil {
			t.Fatal(err)
		}
		if store, err := events.NewOwnedPostgresStore(f.ctx, wrongURL.String(), "token_schema_owner"); err == nil {
			store.Close()
			t.Fatal("Token outbox without the publication fence was accepted")
		}
	})
	t.Run("publish-confirmed", func(t *testing.T) {
		ownerExec := func(sql string) {
			t.Helper()
			if _, err := f.owner.Exec(f.ctx, "SET ROLE token_schema_owner; "+sql+"; SET ROLE token_gateway"); err != nil {
				t.Fatal("owned confirmation fault fixture", err)
			}
		}
		// A sequence is a nontransactional observation barrier: a failed UPDATE
		// cannot erase proof that the real relay attempted local confirmation.
		ownerExec(`CREATE SEQUENCE token_relay_confirmation_probe;
 GRANT USAGE,SELECT ON SEQUENCE token_relay_confirmation_probe TO token_gateway;
 CREATE FUNCTION token_relay_confirmation_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 PERFORM nextval('public.token_relay_confirmation_probe');
 RAISE EXCEPTION 'controlled local confirmation failure' USING ERRCODE='PZ001'; END $$;
 CREATE TRIGGER token_relay_confirmation_fault BEFORE UPDATE OF published_at ON public.outbox_events
 FOR EACH ROW WHEN (NEW.published_at IS NOT NULL) EXECUTE FUNCTION token_relay_confirmation_fault()`)
		start := func() func() {
			t.Helper()
			ctx, cancel := context.WithCancel(f.ctx)
			done := make(chan error, 1)
			go func() { done <- run(ctx, func(key string) string { return environment[key] }, logger) }()
			var once sync.Once
			stop := func() {
				once.Do(func() {
					cancel()
					select {
					case err := <-done:
						if err != nil {
							t.Error("Token relay exit", err)
						}
					case <-time.After(5 * time.Second):
						t.Error("Token relay did not stop")
					}
				})
			}
			t.Cleanup(stop)
			return stop
		}
		stopFirst := start()
		client := &http.Client{Timeout: time.Second}
		wait(t, "real runtime ready", func() bool {
			response, err := client.Get("http://" + environment["EVENT_RELAY_HTTP_ADDR"] + "/health/ready")
			if err != nil {
				return false
			}
			response.Body.Close()
			return response.StatusCode == http.StatusOK
		})
		wait(t, "broker acknowledged before failed database confirmation", func() bool {
			var attempted bool
			err := f.pool.QueryRow(f.ctx, `SELECT is_called FROM token_relay_confirmation_probe`).Scan(&attempted)
			return err == nil && attempted
		})
		stopFirst()
		var pending bool
		if err := f.pool.QueryRow(f.ctx, `SELECT published_at IS NULL AND attempts=0 AND locked_by IS NOT NULL AND publication_generation=0 FROM outbox_events WHERE event_id=$1`, eventID).Scan(&pending); err != nil || !pending {
			t.Fatal("unknown local confirmation lost pending event or fence", err)
		}
		infoBefore, err := stream.Info(f.ctx)
		if err != nil || infoBefore.State.Msgs != 2 {
			t.Fatal("acknowledged original event missing", err)
		}
		ownerExec(`DROP TRIGGER token_relay_confirmation_fault ON public.outbox_events;
 DROP FUNCTION token_relay_confirmation_fault(); DROP SEQUENCE token_relay_confirmation_probe`)
		// Advance only the owned delivery lease, then start a different worker.
		// This is a controlled expiry, not an assertion that zero funds moved.
		if _, err := f.pool.Exec(f.ctx, `UPDATE outbox_events SET locked_until=now()-interval '1 second' WHERE event_id=$1`, eventID); err != nil {
			t.Fatal(err)
		}
		stopRecovered := start()
		defer stopRecovered()
		wait(t, "original domain envelope confirmed", func() bool {
			var confirmed bool
			err := f.pool.QueryRow(f.ctx, `SELECT published_at IS NOT NULL FROM outbox_events WHERE event_id=$1`, eventID).Scan(&confirmed)
			return err == nil && confirmed
		})
		info, err := stream.Info(f.ctx)
		if err != nil || info.State.Msgs != 2 || info.State.LastSeq != infoBefore.State.LastSeq {
			t.Fatal("real domain publication count", info, err)
		}
		message, err := stream.GetMsg(f.ctx, info.State.LastSeq)
		if err != nil || message.Subject != "token.manual-review.created.v1" {
			t.Fatal("original domain subject not carried", err)
		}
		var expected, actual map[string]any
		if json.Unmarshal([]byte(payload), &expected) != nil || json.Unmarshal(message.Data, &actual) != nil {
			t.Fatal("original domain envelope is not JSON")
		}
		// RFC3339 fractional precision is not a domain field; compare its instant.
		expectedTime, e1 := time.Parse(time.RFC3339Nano, expected["occurred_at"].(string))
		actualTime, e2 := time.Parse(time.RFC3339Nano, actual["occurred_at"].(string))
		delete(expected, "occurred_at")
		delete(actual, "occurred_at")
		if e1 != nil || e2 != nil || !expectedTime.Equal(actualTime) || !reflect.DeepEqual(expected, actual) {
			t.Fatal("original domain envelope changed in transport")
		}
		if immutable() != before {
			t.Fatal("event transport changed original case or source state")
		}
	})
}
