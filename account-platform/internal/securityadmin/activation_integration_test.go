package securityadmin

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"github.com/Pinvou/pinvou3/account-platform/internal/identityadapter"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type retainedSecurityDirectory struct {
	subject             string
	activates, disables int
}

func (d *retainedSecurityDirectory) EnsureBootstrapAdmin(context.Context, string) (string, error) {
	return d.subject, nil
}
func (d *retainedSecurityDirectory) ActivateBootstrapAdmin(context.Context, string, string, string) error {
	d.activates++
	return nil
}
func (d *retainedSecurityDirectory) DisableUserAndConfirm(context.Context, string) error {
	d.disables++
	return nil
}

type retainedSecurityFixture struct {
	ctx                   context.Context
	accountURL, walletURL string
	connection            *pgx.Conn
	directory             *retainedSecurityDirectory
	manager               *Manager
}

func retainedSecuritySetup(t *testing.T) *retainedSecurityFixture {
	t.Helper()
	raw := os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL")
	if raw == "" {
		t.Skip("PINVOU_TEST_ACCOUNT_DATABASE_URL is required for real security Account/Billing PostgreSQL integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	control, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal("fixture database connection failed")
	}
	t.Cleanup(func() { control.Close(context.Background()) })
	database := "history_security_db_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	databaseIdentifier := pgx.Identifier{database}.Sanitize()
	if _, err = control.Exec(ctx, "CREATE DATABASE "+databaseIdentifier); err != nil {
		t.Fatal("owned database creation failed")
	}
	t.Cleanup(func() {
		clean, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		if _, e := control.Exec(clean, "DROP DATABASE "+databaseIdentifier); e != nil {
			t.Error("owned database cleanup failed")
			return
		}
		var exists bool
		if e := control.QueryRow(clean, `SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)`, database).Scan(&exists); e != nil || exists {
			t.Error("owned database remains")
		}
	})
	initial, e := pgx.ParseConfig(raw)
	if e != nil {
		t.Fatal("owned database configuration failed")
	}
	initial.Database = database
	admin, err := pgx.ConnectConfig(ctx, initial)
	if err != nil {
		t.Fatal("owned database connection failed")
	}
	t.Cleanup(func() { admin.Close(context.Background()) })
	schemas := []string{"history_security_" + strings.ReplaceAll(uuid.NewString(), "-", ""), "history_wallet_" + strings.ReplaceAll(uuid.NewString(), "-", "")}
	urls := make([]string, 2)
	for i, schema := range schemas {
		quoted := pgx.Identifier{schema}.Sanitize()
		if _, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
			t.Fatal("owned schema creation failed")
		}
		t.Cleanup(func() {
			clean, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			if _, e := admin.Exec(clean, "DROP SCHEMA "+quoted+" CASCADE"); e != nil {
				t.Error("owned schema cleanup failed")
				return
			}
			var exists bool
			if e := admin.QueryRow(clean, `SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname=$1)`, schema).Scan(&exists); e != nil || exists {
				t.Error("owned schema remains")
			}
		})
		configuration, e := pgx.ParseConfig(raw)
		if e != nil {
			t.Fatal("fixture URL configuration failed")
		}
		configuration.Database = database
		configuration.RuntimeParams["search_path"] = schema + ",public"
		ownedURL, e := url.Parse(raw)
		if e != nil {
			t.Fatal("owned URL parse failed")
		}
		ownedURL.Path = "/" + database
		ownedQuery := ownedURL.Query()
		ownedQuery.Set("search_path", schema+",public")
		ownedURL.RawQuery = ownedQuery.Encode()
		urls[i] = ownedURL.String()
		connection, e := pgx.ConnectConfig(ctx, configuration)
		if e != nil {
			t.Fatal("owned schema connection failed")
		}
		directory := "../../migrations/account"
		if i == 1 {
			directory = "../../migrations/billing"
		}
		selected, e := migrations.Load(directory)
		if e != nil {
			t.Fatal(e)
		}
		if i == 0 {
			e = accountschema.Up(ctx, connection, selected)
		} else {
			e = migrations.Up(ctx, connection, selected)
		}
		connection.Close(context.Background())
		if e != nil {
			t.Fatal(e)
		}
	}
	connection, err := pgx.Connect(ctx, urls[0])
	if err != nil {
		t.Fatal("owned account connection failed")
	}
	t.Cleanup(func() { connection.Close(context.Background()) })
	directory := &retainedSecurityDirectory{subject: uuid.NewString()}
	manager, err := NewManager(ctx, urls[0], "https://history-security.invalid", directory)
	if err != nil {
		t.Fatal("manager construction failed")
	}
	t.Cleanup(manager.Close)
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	actualWalletRepository, err := billing.NewPostgresRepository(ctx, urls[1])
	if err != nil {
		t.Fatal("actual wallet producer construction failed")
	}
	t.Cleanup(actualWalletRepository.Close)
	actualWalletHTTP := httptest.NewServer(billing.NewService(actualWalletRepository).Handler("history-owned-workload-credential"))
	t.Cleanup(actualWalletHTTP.Close)
	walletProvisioner, err := account.NewHTTPWalletProvisioner(actualWalletHTTP.URL, "history-owned-workload-credential")
	if err != nil {
		t.Fatal("current wallet client construction failed")
	}
	manager.WithWalletProvisioner(walletProvisioner)
	return &retainedSecurityFixture{ctx, urls[0], urls[1], connection, directory, manager}
}

func (f *retainedSecurityFixture) activate(t *testing.T) uuid.UUID {
	t.Helper()
	issue, e := f.manager.IssueBootstrap(f.ctx, "history-owned-deployment", "INTERACTIVE_SET_PASSWORD", 15*time.Minute)
	if e != nil || issue.Activated {
		t.Fatalf("bootstrap issuance failed: %v", e)
	}
	if e = f.manager.ActivateInteractive(f.ctx, "history-owned-deployment", "S3cure-Quartz-71"); e != nil {
		t.Fatal(e)
	}
	var userID uuid.UUID
	if e = f.connection.QueryRow(f.ctx, `SELECT id FROM users WHERE username='admin'`).Scan(&userID); e != nil {
		t.Fatal(e)
	}
	return userID
}

func TestRetainedSecurityBootstrapNeedsActualWallet(t *testing.T) {
	f := retainedSecuritySetup(t)
	userID := f.activate(t)
	repository, e := account.NewPostgresRepository(f.ctx, f.accountURL)
	if e != nil {
		t.Fatal("account repository construction failed")
	}
	t.Cleanup(repository.Close)
	principal := identity.Principal{Issuer: "https://history-security.invalid", Subject: f.directory.subject, SessionID: uuid.NewString(), Username: "admin"}
	server := httptest.NewServer(account.NewService(repository, nil).Handler(func(context.Context, string) (identity.Principal, error) { return principal, nil }))
	t.Cleanup(server.Close)
	request, e := http.NewRequestWithContext(f.ctx, http.MethodPost, server.URL+"/v1/account/bootstrap", strings.NewReader(`{}`))
	if e != nil {
		t.Fatal(e)
	}
	request.Header.Set("Authorization", "Bearer controlled-fixture")
	request.Header.Set("Content-Type", "application/json")
	response, e := server.Client().Do(request)
	if e != nil {
		t.Fatal("actual account HTTP failed")
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("account bootstrap HTTP=%d", response.StatusCode)
	}
	var status string
	var jobs int
	if e = f.connection.QueryRow(f.ctx, `SELECT status,(SELECT count(*) FROM wallet_provisioning_jobs WHERE user_id=$1 AND status='COMPLETED' AND completed_at IS NOT NULL) FROM users WHERE id=$1`, userID).Scan(&status, &jobs); e != nil {
		t.Fatal(e)
	}
	walletConnection, e := pgx.Connect(f.ctx, f.walletURL)
	if e != nil {
		t.Fatal("wallet fixture connection failed")
	}
	defer walletConnection.Close(context.Background())
	var wallets int
	if e = walletConnection.QueryRow(f.ctx, `SELECT count(*) FROM wallets WHERE owner_type='PERSONAL' AND owner_id=$1`, userID).Scan(&wallets); e != nil {
		t.Fatal(e)
	}
	// Positive control uses the actual Billing PG producer. Capture the original
	// owner row count first, then require idempotent ensure to preserve that owner.
	walletRepository, e := billing.NewPostgresRepository(f.ctx, f.walletURL)
	if e != nil {
		t.Fatal("wallet repository construction failed")
	}
	defer walletRepository.Close()
	wallet, created, e := walletRepository.EnsureWallet(f.ctx, "PERSONAL", userID)
	if e != nil || created != (wallets == 0) || wallet.OwnerID != userID || wallet.Status != "ACTIVE" {
		t.Fatal("actual same-owner wallet control failed")
	}
	if status != "ACTIVE" || jobs != 1 || wallets != 1 {
		t.Fatalf("activated account lacked confirmed wallet facts: status=%s completed_jobs=%d actual_wallets=%d; actual producer positive control passed", status, jobs, wallets)
	}
}

func TestRetainedSecurityBreakGlassReplayHasNoIdentityEffects(t *testing.T) {
	f := retainedSecuritySetup(t)
	f.activate(t)
	public, private, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	verifier, e := NewBreakGlassCredentialVerifier(base64.RawURLEncoding.EncodeToString(public))
	if e != nil {
		t.Fatal(e)
	}
	f.manager.WithBreakGlassVerifier(verifier)
	now := time.Now().UTC()
	payload, e := json.Marshal(BreakGlassIdentity{CredentialID: "history-operator-" + uuid.NewString(), Subject: "history-owned-operator", TargetUsername: "admin", Audience: breakGlassAudience, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(5 * time.Minute)})
	if e != nil {
		t.Fatal(e)
	}
	envelope, e := json.Marshal(breakGlassCredentialEnvelope{Payload: base64.RawURLEncoding.EncodeToString(payload), Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, payload))})
	if e != nil {
		t.Fatal(e)
	}
	input := BreakGlassRequest{OperatorCredential: string(envelope), IncidentTicket: "history-ticket", Reason: "controlled recovery evidence for private fixture", Evidence: []byte("controlled-nonsecret-evidence"), Confirmation: "ALL_SUPER_ADMINS_UNREACHABLE", Duration: 10 * time.Minute, FormalPassword: "S3cure-Quartz-71"}
	if e = f.manager.BreakGlass(f.ctx, input); e != nil {
		t.Fatal(e)
	}
	beforeActivate, beforeDisable := f.directory.activates, f.directory.disables
	input.FormalPassword = "An0ther-Quartz-72"
	replayErr := f.manager.BreakGlass(f.ctx, input)
	var operations int
	if e = f.connection.QueryRow(f.ctx, `SELECT count(*) FROM break_glass_operations`).Scan(&operations); e != nil {
		t.Fatal(e)
	}
	if operations != 1 {
		t.Fatalf("replay changed durable operation count=%d", operations)
	}
	if replayErr == nil || f.directory.activates != beforeActivate || f.directory.disables != beforeDisable {
		t.Fatalf("duplicate operator credential caused identity effects: rejected=%t activation_delta=%d disable_delta=%d durable_operations=%d", replayErr != nil, f.directory.activates-beforeActivate, f.directory.disables-beforeDisable, operations)
	}
}

type retainedPartialIdentity struct {
	mu                                       sync.Mutex
	enabled, failLogout                      bool
	resets, enables, disables, failedLogouts int
}

func retainedRealIdentity(t *testing.T, f *retainedSecurityFixture) *retainedPartialIdentity {
	t.Helper()
	state := &retainedPartialIdentity{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state.mu.Lock()
		defer state.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			io.WriteString(w, `{"access_token":"fixture-token","expires_in":300}`)
			return
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/users") {
			json.NewEncoder(w).Encode([]map[string]any{{"id": f.directory.subject, "username": "admin", "attributes": map[string][]string{"pinvou_bootstrap_managed": {"true"}}}})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/reset-password") && r.Method == http.MethodPut {
			state.resets++
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/logout") && r.Method == http.MethodPost {
			if state.failLogout && state.enabled {
				state.failLogout = false
				state.failedLogouts++
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/users/"+f.directory.subject) {
			if r.Method == http.MethodGet {
				json.NewEncoder(w).Encode(map[string]any{"id": f.directory.subject, "username": "admin", "enabled": state.enabled})
				return
			}
			if r.Method == http.MethodPut {
				var body struct {
					Enabled *bool `json:"enabled"`
				}
				if json.NewDecoder(r.Body).Decode(&body) != nil || body.Enabled == nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				state.enabled = *body.Enabled
				if state.enabled {
					state.enables++
				} else {
					state.disables++
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	directory, e := identityadapter.NewKeycloakDirectory(server.URL, server.URL+"/token", "fixture-realm", "fixture-client", "fixture-credential")
	if e != nil {
		t.Fatal("actual Keycloak adapter construction failed")
	}
	f.manager.directory = directory
	return state
}
func retainedSignedRecovery(t *testing.T, f *retainedSecurityFixture) BreakGlassRequest {
	t.Helper()
	public, private, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	verifier, e := NewBreakGlassCredentialVerifier(base64.RawURLEncoding.EncodeToString(public))
	if e != nil {
		t.Fatal(e)
	}
	f.manager.WithBreakGlassVerifier(verifier)
	now := time.Now().UTC()
	payload, e := json.Marshal(BreakGlassIdentity{CredentialID: "history-partial-" + uuid.NewString(), Subject: "history-owned-operator", TargetUsername: "admin", Audience: breakGlassAudience, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(5 * time.Minute)})
	if e != nil {
		t.Fatal(e)
	}
	envelope, e := json.Marshal(breakGlassCredentialEnvelope{Payload: base64.RawURLEncoding.EncodeToString(payload), Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, payload))})
	if e != nil {
		t.Fatal(e)
	}
	return BreakGlassRequest{OperatorCredential: string(envelope), IncidentTicket: "history-ticket", Reason: "controlled recovery evidence for private fixture", Evidence: []byte("controlled-nonsecret-evidence"), Confirmation: "ALL_SUPER_ADMINS_UNREACHABLE", Duration: 10 * time.Minute, FormalPassword: "S3cure-Quartz-71"}
}
func TestRetainedSecurityPartialBootstrapCompensates(t *testing.T) {
	f := retainedSecuritySetup(t)
	state := retainedRealIdentity(t, f)
	issue, e := f.manager.IssueBootstrap(f.ctx, "history-owned-deployment", "INTERACTIVE_SET_PASSWORD", 15*time.Minute)
	if e != nil || issue.Activated {
		t.Fatal("actual adapter issuance failed")
	}
	state.mu.Lock()
	state.failLogout = true
	beforeDisable := state.disables
	state.mu.Unlock()
	e = f.manager.ActivateInteractive(f.ctx, "history-owned-deployment", "S3cure-Quartz-71")
	if e == nil {
		t.Fatal("expected controlled post-enable logout failure")
	}
	var status, credential string
	var jobs int
	if e = f.connection.QueryRow(f.ctx, `SELECT u.status,c.status,(SELECT count(*) FROM wallet_provisioning_jobs j WHERE j.user_id=u.id) FROM users u JOIN super_admin_bootstrap_credentials c ON c.user_id=u.id WHERE u.username='admin'`).Scan(&status, &credential, &jobs); e != nil {
		t.Fatal("actual rollback facts read failed")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if status != "BOOTSTRAP_PENDING" || credential != "PENDING" || jobs != 0 {
		t.Fatal("account rollback facts changed")
	}
	if state.enabled || state.disables-beforeDisable != 1 || state.resets != 1 || state.enables != 1 || state.failedLogouts != 1 {
		t.Fatalf("partial bootstrap Identity remained enabled: enabled=%t reset=%d enable=%d failed_logout=%d compensation_disable=%d; local pending rollback confirmed", state.enabled, state.resets, state.enables, state.failedLogouts, state.disables-beforeDisable)
	}
}
func TestRetainedSecurityPartialBreakGlassCompensates(t *testing.T) {
	f := retainedSecuritySetup(t)
	state := retainedRealIdentity(t, f)
	f.activate(t)
	request := retainedSignedRecovery(t, f)
	var version int64
	if e := f.connection.QueryRow(f.ctx, `SELECT version FROM users WHERE username='admin'`).Scan(&version); e != nil {
		t.Fatal("baseline version read failed")
	}
	state.mu.Lock()
	state.failLogout = true
	beforeDisable, beforeReset, beforeEnable := state.disables, state.resets, state.enables
	state.mu.Unlock()
	if e := f.manager.BreakGlass(f.ctx, request); e == nil {
		t.Fatal("expected controlled post-enable logout failure")
	}
	var after int64
	var operations int
	if e := f.connection.QueryRow(f.ctx, `SELECT version,(SELECT count(*) FROM break_glass_operations) FROM users WHERE username='admin'`).Scan(&after, &operations); e != nil {
		t.Fatal("actual rollback facts read failed")
	}
	if after != version || operations != 0 {
		t.Fatal("breakglass local transaction did not roll back")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.enabled || state.disables-beforeDisable != 1 || state.resets-beforeReset != 1 || state.enables-beforeEnable != 1 || state.failedLogouts != 1 {
		t.Fatalf("partial recovery Identity remained enabled: enabled=%t reset_delta=%d enable_delta=%d failed_logout=%d compensation_disable=%d; original local version and no receipt confirmed", state.enabled, state.resets-beforeReset, state.enables-beforeEnable, state.failedLogouts, state.disables-beforeDisable)
	}
}

// Both modes drop an actual server COMMIT completion. The unavailable mode
// additionally refuses the independent outcome-check connection.
func retainedCommitACKProxy(t *testing.T, target string) (string, *atomic.Bool, *atomic.Int32) {
	return retainedCommitACKProxyMode(t, target, false)
}
func retainedCommitACKProxyUnavailable(t *testing.T, target string) (string, *atomic.Bool, *atomic.Int32) {
	return retainedCommitACKProxyMode(t, target, true)
}
func retainedCommitACKProxyMode(t *testing.T, target string, unavailable bool) (string, *atomic.Bool, *atomic.Int32) {
	t.Helper()
	parsed, e := url.Parse(target)
	if e != nil {
		t.Fatal("owned proxy target invalid")
	}
	backendAddress := parsed.Host
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal("owned proxy listen failed")
	}
	armed := &atomic.Bool{}
	drops := &atomic.Int32{}
	var mu sync.Mutex
	active := map[net.Conn]bool{}
	var connections sync.WaitGroup
	acceptDone := make(chan struct{})
	t.Cleanup(func() {
		listener.Close()
		// Every successful Accept has registered its connection before this closes.
		<-acceptDone
		mu.Lock()
		for c := range active {
			c.Close()
		}
		mu.Unlock()
		connections.Wait()
	})
	go func() {
		defer close(acceptDone)
		for {
			client, e := listener.Accept()
			if e != nil {
				return
			}
			mu.Lock()
			active[client] = true
			mu.Unlock()
			connections.Add(1)
			go func() {
				defer connections.Done()
				defer func() { client.Close(); mu.Lock(); delete(active, client); mu.Unlock() }()
				backend, e := net.DialTimeout("tcp", backendAddress, 5*time.Second)
				if e != nil {
					return
				}
				defer backend.Close()
				client.SetDeadline(time.Now().Add(30 * time.Second))
				backend.SetDeadline(time.Now().Add(30 * time.Second))
				copyDone := make(chan struct{})
				go func() {
					defer close(copyDone)
					io.Copy(backend, client)
					backend.Close()
				}()
				// Close both directions to unblock the forwarding goroutine,
				// and wait for it before releasing this connection's ownership.
				defer func() {
					client.Close()
					backend.Close()
					<-copyDone
				}()
				header := make([]byte, 5)
				for {
					if _, e = io.ReadFull(backend, header); e != nil {
						return
					}
					length := binary.BigEndian.Uint32(header[1:])
					if length < 4 || length > 1<<20 {
						return
					}
					body := make([]byte, int(length)-4)
					if _, e = io.ReadFull(backend, body); e != nil {
						return
					}
					if header[0] == 'C' && string(body) == "COMMIT\x00" && armed.CompareAndSwap(true, false) {
						drops.Add(1)
						if unavailable {
							listener.Close()
						}
						return
					}
					if _, e = client.Write(header); e != nil {
						return
					}
					if _, e = client.Write(body); e != nil {
						return
					}
				}
			}()
		}
	}()
	parsed.Host = listener.Addr().String()
	return parsed.String(), armed, drops
}
func TestRetainedSecurityLostCommitACKKeepsCommittedIdentity(t *testing.T) {
	f := retainedSecuritySetup(t)
	state := retainedRealIdentity(t, f)
	proxyURL, armed, drops := retainedCommitACKProxy(t, f.accountURL)
	original := f.manager
	manager, e := NewManager(f.ctx, proxyURL, "https://history-security.invalid", original.directory)
	if e != nil {
		t.Fatal("actual proxied manager construction failed")
	}
	t.Cleanup(manager.Close)
	manager.WithWalletProvisioner(original.wallets)
	f.manager = manager
	issue, e := manager.IssueBootstrap(f.ctx, "history-owned-deployment", "INTERACTIVE_SET_PASSWORD", 15*time.Minute)
	if e != nil || issue.Activated {
		t.Fatal("owned bootstrap issuance failed")
	}
	armed.Store(true)
	activationErr := manager.ActivateInteractive(f.ctx, "history-owned-deployment", "S3cure-Quartz-71")
	var status, credential string
	var jobs int
	if e = f.connection.QueryRow(f.ctx, `SELECT u.status,c.status,(SELECT count(*) FROM wallet_provisioning_jobs j WHERE j.user_id=u.id AND j.status='COMPLETED') FROM users u JOIN super_admin_bootstrap_credentials c ON c.user_id=u.id WHERE u.username='admin'`).Scan(&status, &credential, &jobs); e != nil {
		t.Fatal("committed authority facts read failed")
	}
	if drops.Load() != 1 || status != "ACTIVE" || credential != "CONSUMED" || jobs != 1 {
		t.Fatalf("expected exact committed ACK loss not exercised: drops=%d active=%t consumed=%t completed_jobs=%d", drops.Load(), status == "ACTIVE", credential == "CONSUMED", jobs)
	}
	replay, e := manager.IssueBootstrap(f.ctx, "history-owned-deployment", "INTERACTIVE_SET_PASSWORD", 15*time.Minute)
	if e != nil || !replay.Activated {
		t.Fatal("current already-active recovery path did not read durable activation")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if activationErr != nil || !state.enabled || state.resets != 1 || state.enables != 1 {
		t.Fatalf("committed activation was compensated after lost ACK: enabled=%t reset=%d enable=%d disables=%d original_error=%t already_active_reentry=true", state.enabled, state.resets, state.enables, state.disables, activationErr != nil)
	}
}

func TestRetainedSecurityLostBreakGlassCommitACKKeepsCommittedIdentity(t *testing.T) {
	f := retainedSecuritySetup(t)
	state := retainedRealIdentity(t, f)
	f.activate(t)
	proxyURL, armed, drops := retainedCommitACKProxy(t, f.accountURL)
	original := f.manager
	manager, e := NewManager(f.ctx, proxyURL, "https://history-security.invalid", original.directory)
	if e != nil {
		t.Fatal("actual proxied manager construction failed")
	}
	t.Cleanup(manager.Close)
	manager.WithWalletProvisioner(original.wallets)
	f.manager = manager
	request := retainedSignedRecovery(t, f)
	armed.Store(true)
	recoveryErr := manager.BreakGlass(f.ctx, request)
	var operations int
	var status string
	if e = f.connection.QueryRow(f.ctx, `SELECT status,(SELECT count(*) FROM break_glass_operations) FROM users WHERE username='admin'`).Scan(&status, &operations); e != nil {
		t.Fatal("committed recovery authority read failed")
	}
	if drops.Load() != 1 || status != "ACTIVE" || operations != 1 {
		t.Fatalf("expected committed recovery ACK loss not exercised: drops=%d active=%t operations=%d", drops.Load(), status == "ACTIVE", operations)
	}
	state.mu.Lock()
	beforeReset, beforeEnable := state.resets, state.enables
	state.mu.Unlock()
	_ = manager.BreakGlass(f.ctx, request)
	state.mu.Lock()
	defer state.mu.Unlock()
	if recoveryErr != nil || !state.enabled || state.resets != beforeReset || state.enables != beforeEnable {
		t.Fatalf("committed recovery was compensated after lost ACK: enabled=%t reset_delta=%d enable_delta=%d original_error=%t same_credential_reentry_completed=true", state.enabled, state.resets-beforeReset, state.enables-beforeEnable, recoveryErr != nil)
	}
}

func TestRetainedSecurityLostCommitACKUnavailableReadIsNotSuccess(t *testing.T) {
	f := retainedSecuritySetup(t)
	state := retainedRealIdentity(t, f)
	proxyURL, armed, drops := retainedCommitACKProxyUnavailable(t, f.accountURL)
	original := f.manager
	manager, e := NewManager(f.ctx, proxyURL, "https://history-security.invalid", original.directory)
	if e != nil {
		t.Fatal("actual proxied manager construction failed")
	}
	t.Cleanup(manager.Close)
	manager.WithWalletProvisioner(original.wallets)
	f.manager = manager
	issue, e := manager.IssueBootstrap(f.ctx, "history-owned-deployment", "INTERACTIVE_SET_PASSWORD", 15*time.Minute)
	if e != nil || issue.Activated {
		t.Fatal("owned bootstrap issuance failed")
	}
	armed.Store(true)
	activationErr := manager.ActivateInteractive(f.ctx, "history-owned-deployment", "S3cure-Quartz-71")
	var status, credential string
	var jobs int
	if e = f.connection.QueryRow(f.ctx, `SELECT u.status,c.status,(SELECT count(*) FROM wallet_provisioning_jobs j WHERE j.user_id=u.id AND j.status='COMPLETED') FROM users u JOIN super_admin_bootstrap_credentials c ON c.user_id=u.id WHERE u.username='admin'`).Scan(&status, &credential, &jobs); e != nil {
		t.Fatal("committed authority facts read failed")
	}
	if drops.Load() != 1 || status != "ACTIVE" || credential != "CONSUMED" || jobs != 1 {
		t.Fatalf("expected exact committed ACK loss not exercised: drops=%d active=%t consumed=%t completed_jobs=%d", drops.Load(), status == "ACTIVE", credential == "CONSUMED", jobs)
	}
	if activationErr == nil || !strings.Contains(activationErr.Error(), "SECURITY_ACTIVATION_COMMIT_UNCERTAIN") {
		t.Fatal("unavailable outcome read returned success or lacked explicit uncertainty")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.enabled || state.resets != 1 || state.enables != 1 {
		t.Fatal("unavailable outcome read incorrectly disabled identity or repeated activation")
	}

}

func TestRetainedSecurityLostBreakGlassACKUnavailableReadIsNotSuccess(t *testing.T) {
	f := retainedSecuritySetup(t)
	state := retainedRealIdentity(t, f)
	f.activate(t)
	proxyURL, armed, drops := retainedCommitACKProxyUnavailable(t, f.accountURL)
	original := f.manager
	manager, e := NewManager(f.ctx, proxyURL, "https://history-security.invalid", original.directory)
	if e != nil {
		t.Fatal("actual proxied manager construction failed")
	}
	t.Cleanup(manager.Close)
	manager.WithWalletProvisioner(original.wallets)
	f.manager = manager
	request := retainedSignedRecovery(t, f)
	armed.Store(true)
	recoveryErr := manager.BreakGlass(f.ctx, request)
	var operations int
	var status string
	if e = f.connection.QueryRow(f.ctx, `SELECT status,(SELECT count(*) FROM break_glass_operations) FROM users WHERE username='admin'`).Scan(&status, &operations); e != nil {
		t.Fatal("committed recovery authority read failed")
	}
	if drops.Load() != 1 || status != "ACTIVE" || operations != 1 {
		t.Fatalf("expected committed recovery ACK loss not exercised: drops=%d active=%t operations=%d", drops.Load(), status == "ACTIVE", operations)
	}
	if recoveryErr == nil || !strings.Contains(recoveryErr.Error(), "SECURITY_ACTIVATION_COMMIT_UNCERTAIN") {
		t.Fatal("unavailable recovery outcome read returned success or lacked explicit uncertainty")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.enabled || state.resets != 2 || state.enables != 2 {
		t.Fatal("unavailable recovery outcome read incorrectly disabled identity or repeated activation")
	}

}
