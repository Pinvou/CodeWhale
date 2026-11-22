package securityadmin

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"github.com/Pinvou/pinvou3/account-platform/internal/cifixture"
	"github.com/Pinvou/pinvou3/account-platform/internal/identityadapter"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/billingschema"
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
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	actors := newRetainedSecurityActors(t, ctx, raw)
	urls := make([]string, 2)
	for i, domain := range []string{"account", "billing"} {
		owner := domain + "_service"
		if domain == "billing" {
			owner = "billing_schema_owner"
		}
		c := actors.database(t, ctx, domain, owner)
		selected, err := migrations.Load("../../migrations/" + domain)
		if err != nil {
			t.Fatal(err)
		}
		if domain == "account" {
			err = accountschema.Up(ctx, c, selected)
		} else {
			err = billingschema.Up(ctx, c, selected)
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err = migrations.VerifyApplied(ctx, c, selected); err != nil {
			t.Fatal(err)
		}
		migrations.Unlock(ctx, c)
		urls[i] = actors.serviceURL(t, c, domain+"_service")
	}
	connection, err := pgx.Connect(ctx, urls[0])
	if err != nil {
		t.Fatal("owned account connection failed")
	}
	t.Cleanup(func() { connection.Close(context.Background()) })
	var actor string
	var privileged bool
	if err = connection.QueryRow(ctx, `SELECT current_user,(SELECT rolsuper OR rolcreatedb OR rolcreaterole OR rolbypassrls FROM pg_roles WHERE rolname=current_user)`).Scan(&actor, &privileged); err != nil || actor != "account_service" || privileged {
		t.Fatal("runtime actor is not isolated Account service")
	}
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
	resetFailureStatus                       int
	resetFailureBody                         string
	resetDisconnect                          bool
	resetAttempts, logouts                   int
	mu                                       sync.Mutex
	enabled, failLogout                      bool
	resets, enables, disables, failedLogouts int
	disableEntered                           chan struct{}
	disableContinue                          <-chan struct{}
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
			state.resetAttempts++
			if state.resetFailureStatus != 0 {
				w.WriteHeader(state.resetFailureStatus)
				io.WriteString(w, state.resetFailureBody)
				return
			}
			state.resets++
			if state.resetDisconnect {
				connection, _, _ := w.(http.Hijacker).Hijack()
				connection.Close()
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/logout") && r.Method == http.MethodPost {
			state.logouts++
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
				if !*body.Enabled && state.disableEntered != nil {
					entered, resume := state.disableEntered, state.disableContinue
					state.disableEntered = nil
					close(entered)
					select {
					case <-resume:
					case <-r.Context().Done():
						return
					}
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
	return retainedCommitACKProxyHook(t, target, unavailable, nil)
}
func retainedCommitACKProxyHook(t *testing.T, target string, unavailable bool, hook func()) (string, *atomic.Bool, *atomic.Int32) {
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
						if hook != nil {
							hook()
						}
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

// Isolated current public migrations use the same deployment actor and cleanup
// contract as the audit-export rollback fixture; domain writes use service roles.
type retainedSecurityActors struct {
	admin          *pgx.Conn
	config         *pgx.ConnConfig
	roles          []string
	dbs            map[string]string
	databaseLeases map[string]*cifixture.Database
	connections    []*pgx.Conn
}

func newRetainedSecurityActors(t *testing.T, ctx context.Context, raw string) *retainedSecurityActors {
	t.Helper()
	config, err := pgx.ParseConfig(raw)
	if err != nil || !retainedSecurityConfigurationAllowed(config, os.Getenv("F17_TEST_DATABASE_PROFILE"), os.Getenv("GITHUB_ACTIONS") == "true") {
		t.Fatal("dedicated isolated fixture configuration required")
	}
	admin, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal("connect schema setup identity")
	}
	// Only this branch's fixtures share the deployment-actor set on their isolated instance.
	// The session lock serializes those fixtures across Go packages; it grants no authority.
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	if _, err := admin.Exec(ctx, `SELECT pg_advisory_lock(hashtext('pinvou.security.activation.fixture'))`); err != nil {
		t.Fatal("exclusive fixture lock failed")
	}
	var directory, session, database string
	var port int
	var super bool
	if err := admin.QueryRow(ctx, `SELECT current_setting('data_directory'),session_user,current_database(),current_setting('port')::int,(SELECT rolsuper FROM pg_roles WHERE rolname=session_user)`).Scan(&directory, &session, &database, &port, &super); err != nil || session != config.User || database != "postgres" || port != int(config.Port) || !super {
		t.Fatal("isolated coordinator identity rejected")
	}
	if os.Getenv("F17_TEST_DATABASE_PROFILE") == "github-actions-service" {
		if directory != "/var/lib/postgresql/data" {
			t.Fatal("isolated CI service directory rejected")
		}
	} else if os.Getenv("F17_TEST_DATABASE_PROFILE") == "local-owned" {
		if filepath.Clean(filepath.FromSlash(directory)) != filepath.Clean(os.Getenv("PINVOU_TEST_OWNED_DATA_DIRECTORY")) {
			t.Fatal("owned temporary directory rejected")
		}
	} else if strings.ReplaceAll(directory, "\\", "/") != "D:/Projects/pinvou3-account-functional/.cache/parallel-runtimes/f04/data" {
		t.Fatal("owned local runtime directory rejected")
	}
	var outsideDatabases, outsideActors int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_database WHERE datname NOT IN ('postgres','template0','template1')`).Scan(&outsideDatabases); err != nil || outsideDatabases != 0 {
		t.Fatal("outside databases prohibit fixture writes")
	}
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname !~ '^pg_' AND rolname<>$1`, session).Scan(&outsideActors); err != nil || outsideActors != 0 {
		t.Fatal("outside actors prohibit fixture writes")
	}
	f := &retainedSecurityActors{admin: admin, config: config, dbs: make(map[string]string), roles: []string{"account_service", "account_migrator", "account_approval_control_owner", "account_approval_authorization_owner", "account_approval_writer", "account_approval_backfill", "account_approval_relay", "account_approval_recovery", "billing_service", "billing_schema_owner", "billing_migrator"}}
	var count int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname=ANY($1)`, f.roles).Scan(&count); err != nil || count != 0 {
		admin.Close(context.Background())
		t.Fatal("existing deployment actors: no fixture writes permitted")
	}
	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	for _, role := range f.roles {
		login, inherit := "LOGIN", "NOINHERIT"
		if strings.HasSuffix(role, "_owner") || role == "account_approval_recovery" {
			login = "NOLOGIN"
		}
		if role == "account_service" || role == "billing_service" {
			inherit = "INHERIT"
		}
		if _, err := tx.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" "+login+" "+inherit+" NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS"); err != nil {
			t.Fatal("exclusive actor creation rejected")
		}
	}
	if _, err := tx.Exec(ctx, `GRANT account_service,account_approval_control_owner TO account_migrator; GRANT account_approval_authorization_owner TO account_migrator WITH INHERIT FALSE,SET TRUE,ADMIN TRUE; GRANT billing_service,billing_schema_owner TO billing_migrator; GRANT billing_service TO billing_schema_owner WITH INHERIT TRUE,SET TRUE`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		clean, stop := context.WithTimeout(context.Background(), 45*time.Second)
		defer stop()
		defer admin.Close(clean)
		for _, c := range f.connections {
			if err := c.Close(clean); err != nil {
				t.Error("owned migration connection close failed")
			}
		}
		allGone := true
		for name, owner := range f.dbs {
			if _, err := cifixture.DropDatabase(clean, admin, f.databaseLeases[name], owner); err != nil {
				t.Error("nonforce owned DB cleanup failed")
				allGone = false
			}
		}
		if !allGone {
			t.Error("owned actors retained after incomplete DB cleanup")
			return
		}
		var outside bool
		if err := admin.QueryRow(clean, `SELECT EXISTS(SELECT 1 FROM pg_shdepend WHERE refclassid='pg_authid'::regclass AND refobjid IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1)) AND dbid<>0) OR EXISTS(SELECT 1 FROM pg_auth_members WHERE (roleid IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1)) OR member IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1))) AND NOT(roleid IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1)) AND member IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1))))`, f.roles).Scan(&outside); err != nil || outside {
			t.Error("external dependencies forbid actor cleanup")
			return
		}
		for i := len(f.roles) - 1; i >= 0; i-- {
			if _, err := admin.Exec(clean, "DROP ROLE "+pgx.Identifier{f.roles[i]}.Sanitize()); err != nil {
				t.Error("nonforce owned actor cleanup failed")
			}
		}
		if err := admin.QueryRow(clean, `SELECT count(*) FROM pg_roles WHERE rolname=ANY($1)`, f.roles).Scan(&count); err != nil || count != 0 {
			t.Error("owned actors remain after cleanup")
		}
	})
	return f
}

// CI is a separate ephemeral service profile, never a bypass for an arbitrary
// deployment URL. Local runs additionally verify their exact owned directory.
func retainedSecurityConfigurationAllowed(config *pgx.ConnConfig, profile string, githubActions bool) bool {
	if config == nil || (config.Host != "127.0.0.1" && config.Host != "localhost") || config.Database != "postgres" || len(config.Fallbacks) != 0 {
		return false
	}
	if profile == "local-owned" {
		n, err := strconv.ParseUint(os.Getenv("PINVOU_TEST_OWNED_PORT"), 10, 16)
		directory := os.Getenv("PINVOU_TEST_OWNED_DATA_DIRECTORY")
		return err == nil && n >= 1024 && config.Port == uint16(n) && config.Host == "127.0.0.1" && config.TLSConfig == nil && filepath.IsAbs(directory) && filepath.Base(filepath.Clean(directory)) == "data"
	}
	if profile == "github-actions-service" {
		return githubActions && config.Port == 5432 && config.User == "postgres"
	}
	return profile == "" && config.Port == 55504 && config.User == "pinvou_wave_test"
}

func (f *retainedSecurityActors) database(t *testing.T, ctx context.Context, domain, owner string) *pgx.Conn {
	t.Helper()
	name := "retained_security_" + domain + "_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	fixtureDatabase, creationError := cifixture.CreateDatabase(ctx, f.admin, name, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" OWNER "+pgx.Identifier{owner}.Sanitize())
	if creationError != nil {
		t.Fatal(creationError)
	}
	if f.databaseLeases == nil {
		f.databaseLeases = make(map[string]*cifixture.Database)
	}
	f.databaseLeases[name] = fixtureDatabase
	if err := creationError; err != nil {
		t.Fatal("create exclusively owned DB")
	}
	f.dbs[name] = owner
	t.Logf("owned fixture database=%s owner=%s", name, owner)
	config := f.config.Copy()
	config.Database = name
	if domain == "account" {
		config.User = "account_migrator"
		config.Password = ""
		if _, err := f.admin.Exec(ctx, "GRANT CREATE ON DATABASE "+pgx.Identifier{name}.Sanitize()+" TO account_migrator"); err != nil {
			t.Fatal(err)
		}
	}
	if domain == "billing" {
		config.User = "billing_migrator"
		config.Password = ""
	}
	c, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal("connect isolated migration actor")
	}
	f.connections = append(f.connections, c)
	if domain == "billing" {
		if _, err := c.Exec(ctx, `SET ROLE billing_schema_owner; ALTER SCHEMA public OWNER TO billing_schema_owner; REVOKE CREATE ON SCHEMA public FROM PUBLIC; GRANT USAGE,CREATE ON SCHEMA public TO billing_service`); err != nil {
			t.Fatal(err)
		}
	}
	if domain == "account" || domain == "billing" {
		if _, err := c.Exec(ctx, "SET ROLE "+pgx.Identifier{domain + "_service"}.Sanitize()); err != nil {
			t.Fatal(err)
		}
		var session, current string
		var super bool
		if err := c.QueryRow(ctx, `SELECT session_user,current_user,(SELECT rolsuper FROM pg_roles WHERE rolname=current_user)`).Scan(&session, &current, &super); err != nil || session != domain+"_migrator" || current != domain+"_service" || super {
			t.Fatal("non-super migration actor contract failed")
		}
	}
	return c
}

func (f *retainedSecurityActors) serviceURL(t *testing.T, c *pgx.Conn, user string) string {
	t.Helper()
	parsed, err := url.Parse(f.config.ConnString())
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		t.Fatal("schema fixture requires a PostgreSQL URL")
	}
	parsed.User = url.User(user)
	parsed.Path = "/" + c.Config().Database
	return parsed.String()
}
