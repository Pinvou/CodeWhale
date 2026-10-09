package billingschema

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
	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Actual PostgreSQL control transactions use the existing exclusive actor
// fixture; actual TLS NATS resources are private to this single test. Neither
// a fabricated stream receipt nor an externally deployed broker is used.
func TestFreshApprovalOperatorActualPGTLSNATS(t *testing.T) {
	raw, binary := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL"), os.Getenv("PINVOU_TEST_NATS_SERVER_BINARY")
	if raw == "" || binary == "" {
		t.Skip("explicit owned PostgreSQL and NATS fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, raw)
	all, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal("installed Account migrations unavailable")
	}
	c := f.database(t, ctx, "account", "account_service")
	if err = accountschema.Up(ctx, c, all); err != nil {
		t.Fatal("actual Account schema failed")
	}
	dsn := f.serviceURL(t, c, "account_migrator")
	callback := freshActualResources(t, ctx, binary)
	status, err := account.RunFreshApprovalStep(ctx, dsn, 1, account.FreshApprovalStatus, nil)
	if err != nil || status.Mode != account.AdminApprovalModeLegacy || status.Version != 1 {
		t.Fatal("fresh authoritative status unavailable")
	}
	if _, err = account.RunFreshApprovalStep(ctx, dsn, 2, account.FreshApprovalQuiesce, nil); err == nil {
		t.Fatal("wrong expected version changed control")
	}
	for _, item := range []struct {
		step                            account.FreshApprovalStep
		expected, version, epoch, relay int64
	}{
		{account.FreshApprovalQuiesce, 1, 2, 2, 1}, {account.FreshApprovalFence, 2, 3, 2, 2},
		{account.FreshApprovalResources, 3, 3, 2, 2}, {account.FreshApprovalActivate, 3, 4, 3, 2}, {account.FreshApprovalResume, 4, 5, 3, 3},
	} {
		t.Run(string(item.step), func(t *testing.T) {
			var resources func(context.Context, account.AdminApprovalControl) (json.RawMessage, error)
			if item.step == account.FreshApprovalResources || item.step == account.FreshApprovalActivate || item.step == account.FreshApprovalResume {
				resources = callback
			}
			got, e := account.RunFreshApprovalStep(ctx, dsn, item.expected, item.step, resources)
			if e != nil || got.Version != item.version || got.ApprovalEpoch != item.epoch || got.AuthorizationRelayEpoch != item.relay {
				t.Fatalf("actual operator step failed: %s", item.step)
			}
		})
		if t.Failed() {
			return
		}
	}
	if _, err = account.RunFreshApprovalStep(ctx, dsn, 4, account.FreshApprovalResume, callback); err == nil {
		t.Fatal("stale CAS repeated transition")
	}
	var count int
	if err = c.QueryRow(ctx, `SELECT count(*) FROM public.audit_logs WHERE action='APPROVAL_FRESH_INITIALIZATION'`).Scan(&count); err != nil || count != 5 {
		t.Fatal("immutable operator audit missing or duplicated")
	}
	// A second fresh database has genuine original-schema historical approval
	// facts. They must remain byte-for-byte, including DRAFT requests.
	history := f.database(t, ctx, "account", "account_service")
	if accountschema.Up(ctx, history, all) != nil {
		t.Fatal("history fixture schema failed")
	}
	// The original sparse-upgrade seed also includes an obsolete unrestricted
	// superadmin assignment. This final-schema fixture needs only legitimate
	// Billing operator bindings, not that pre-migration legacy authority.
	seed := strings.Split(stageAccountSeed, "UPDATE authorization_versions")[0]
	seed = strings.ReplaceAll(seed, "PLATFORM_SUPER_ADMIN", "PLATFORM_BILLING_OPERATOR")
	if _, err = history.Exec(ctx, seed); err != nil {
		t.Fatal("original user/binding fixture failed")
	}
	if _, err = history.Exec(ctx, `INSERT INTO approval_requests(id,command_type,policy_version,requester_user_id,requester_binding_id,requester_authorization_version,idempotency_key,normalized_command,command_fingerprint,target_resource_type,target_resource_id,reason,redacted_evidence,expected_impact,status,version,expires_at,created_at,updated_at)
 SELECT $1,'WALLET_ADJUSTMENT',1,ra.user_id,ra.id,1,'controlled-history-key','{}',decode(repeat('00',32),'hex'),'WALLET',$2,'controlled historical approval','[]','{}','DRAFT',1,now()+interval '1 hour',now(),now() FROM role_assignments ra LIMIT 1`, uuid.New(), uuid.NewString()); err != nil {
		t.Fatal("real historical approval fixture rejected")
	}
	before := stageValue(t, ctx, history, `SELECT jsonb_agg(to_jsonb(r))::text FROM approval_requests r`)
	if before == "null" || before == "" {
		t.Fatal("history was not actually stored")
	}
	historyDSN := f.serviceURL(t, history, "account_migrator")
	if _, err = account.RunFreshApprovalStep(ctx, historyDSN, 1, account.FreshApprovalQuiesce, nil); err == nil {
		t.Fatal("historical approval accepted fresh initialization")
	}
	if before != stageValue(t, ctx, history, `SELECT jsonb_agg(to_jsonb(r))::text FROM approval_requests r`) || stageValue(t, ctx, history, `SELECT mode||':'||version FROM account_approval_control.admin_approval_control`) != "LEGACY:1" {
		t.Fatal("blocked fresh initializer changed historical facts/control")
	}
}

func freshActualResources(t *testing.T, ctx context.Context, binary string) func(context.Context, account.AdminApprovalControl) (json.RawMessage, error) {
	t.Helper()
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
		pair := freshNKey(t)
		pairs[role] = pair
		t.Cleanup(pair.Wipe)
		keys[role], _ = pair.PublicKey()
	}
	oldAccount := freshNKey(t)
	t.Cleanup(oldAccount.Wipe)
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
	observer := freshNKey(t)
	t.Cleanup(observer.Wipe)
	observerPublic, _ := observer.PublicKey()
	// Fixture-only observer creates the controlled forbidden legacy object.
	// It is neither in the generated identity nor an issued production role.
	users := auth["authorization"].(map[string]any)["users"].([]any)
	auth["authorization"].(map[string]any)["users"] = append(users, map[string]any{"nkey": observerPublic, "permissions": map[string]any{"publish": []string{"$JS.API.>"}, "subscribe": []string{"_INBOX.cutover-test.>"}}})
	ca, cert, key := freshTLS(t)
	caPath := write(private, "ca.crt", ca)
	certPath := write(private, "server.crt", cert)
	keyPath := write(private, "server.key", key)
	port := freshFreePort(t)
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
	t.Cleanup(func() {
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
	})
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
	t.Cleanup(observerConnection.Close)
	_, err = jetstream.New(observerConnection)
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
	t.Cleanup(broker.Close)
	if broker.Publish(ctx, "account.test.v1", []byte(`{"test":true}`), uuid.NewString()) != nil {
		t.Fatal("ordinary real publication failed")
	}

	var files events.ApprovalConsumerResourceFiles
	var captured account.AdminApprovalControl
	return func(operation context.Context, control account.AdminApprovalControl) (json.RawMessage, error) {
		if files.ReceiptFile == "" {
			receipt, e := events.InitializeFreshApprovalConsumers(operation, url, cfg, identity, manifestPath, configuration.ManifestSHA256, events.FreshApprovalControl{Version: control.Version, ApprovalEpoch: control.ApprovalEpoch, RelayEpoch: control.AuthorizationRelayEpoch})
			if e != nil {
				return nil, e
			}
			var wire struct {
				HighWatermark string `json:"high_watermark"`
				BillingStarts []struct {
					StartSequence string `json:"start_sequence"`
				} `json:"billing_starts"`
			}
			if json.Unmarshal(receipt, &wire) != nil || wire.HighWatermark != "1" || len(wire.BillingStarts) != 3 {
				return nil, events.ErrNATSResources
			}
			for _, s := range wire.BillingStarts {
				if s.StartSequence != "2" {
					return nil, events.ErrNATSResources
				}
			}
			receiptPath := write(resources, "receipt.json", receipt)
			sum := sha256.Sum256(receipt)
			files = events.ApprovalConsumerResourceFiles{Identity: identity, ManifestFile: manifestPath, ManifestSHA256: configuration.ManifestSHA256, ReceiptFile: receiptPath, ReceiptSHA256: hex.EncodeToString(sum[:])}
			captured = control
			return receipt, nil
		}
		facts, e := events.CheckFreshApprovalConsumers(operation, url, cfg, files)
		if e != nil || facts.Version != captured.Version || facts.ApprovalEpoch != captured.ApprovalEpoch || facts.RelayEpoch != captured.AuthorizationRelayEpoch {
			return nil, events.ErrNATSResources
		}
		if control.Version != captured.Version && control.Version != captured.Version+1 {
			return nil, events.ErrNATSResources
		}
		return files.VerifiedReceipt(time.Now().UTC())
	}
}
func freshNKey(t *testing.T) nkeys.KeyPair {
	t.Helper()
	pair, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal("controlled NKey generation failed")
	}
	return pair
}
func stringsToBytes(value string) []byte { return []byte(value) }
func freshFreePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("owned loopback port failed")
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}
func freshTLS(t *testing.T) ([]byte, []byte, []byte) {
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
