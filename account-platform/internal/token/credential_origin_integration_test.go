package token

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	adminportal "github.com/Pinvou/pinvou3/account-platform/internal/portal"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type controlledCredentialIDP struct {
	server    *httptest.Server
	key       *rsa.PrivateKey
	actor     uuid.UUID
	audience  string
	mu        sync.Mutex
	challenge string
	nonce     string
	redirect  string
	code      string
	tokenHits atomic.Int64
}

func newControlledCredentialIDP(t *testing.T, actor uuid.UUID, audience string) *controlledCredentialIDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &controlledCredentialIDP{key: key, actor: actor, audience: audience, code: "credential-code"}
	idp.server = httptest.NewServer(http.HandlerFunc(idp.serveHTTP))
	t.Cleanup(idp.server.Close)
	return idp
}

func (idp *controlledCredentialIDP) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/jwks":
		n := base64.RawURLEncoding.EncodeToString(idp.key.PublicKey.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1})
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "credential-test", "n": n, "e": e}}})
	case "/authorize":
		query := r.URL.Query()
		if query.Get("client_id") != idp.audience || query.Get("response_type") != "code" || query.Get("code_challenge_method") != "S256" || query.Get("state") == "" || query.Get("nonce") == "" || query.Get("code_challenge") == "" {
			http.Error(w, "invalid authorization", http.StatusBadRequest)
			return
		}
		idp.mu.Lock()
		idp.challenge, idp.nonce, idp.redirect = query.Get("code_challenge"), query.Get("nonce"), query.Get("redirect_uri")
		idp.mu.Unlock()
		target, _ := url.Parse(idp.redirect)
		values := target.Query()
		values.Set("code", idp.code)
		values.Set("state", query.Get("state"))
		target.RawQuery = values.Encode()
		http.Redirect(w, r, target.String(), http.StatusSeeOther)
	case "/token":
		idp.tokenHits.Add(1)
		if err := r.ParseForm(); err != nil || r.Form.Get("code") != idp.code || r.Form.Get("client_id") != idp.audience || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("redirect_uri") != idp.redirect {
			http.Error(w, "invalid token request", http.StatusBadRequest)
			return
		}
		challenge := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(challenge[:]) != idp.challenge {
			http.Error(w, "invalid pkce", http.StatusBadRequest)
			return
		}
		now := time.Now().UTC()
		claims := map[string]any{"iss": idp.server.URL, "sub": idp.actor.String(), "aud": idp.audience, "exp": now.Add(time.Minute).Unix(), "iat": now.Unix(), "auth_time": now.Unix(), "nonce": idp.nonce, "sid": "credential-session"}
		raw, err := signCredentialTestJWT(idp.key, claims)
		if err != nil {
			http.Error(w, "signing failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"id_token": raw, "token_type": "Bearer"})
	default:
		http.NotFound(w, r)
	}
}

func signCredentialTestJWT(key *rsa.PrivateKey, claims map[string]any) (string, error) {
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "credential-test", "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func credentialHiddenValue(t *testing.T, body, name string) string {
	t.Helper()
	match := regexp.MustCompile(`name="` + regexp.QuoteMeta(name) + `" value="([^"]+)"`).FindStringSubmatch(body)
	if len(match) != 2 {
		t.Fatalf("credential form field %s is missing: %s", name, body)
	}
	return match[1]
}

func credentialHTTP(t *testing.T, client *http.Client, method, target string, form url.Values, origin string) *http.Response {
	t.Helper()
	var body *strings.Reader
	if form == nil {
		body = strings.NewReader("")
	} else {
		body = strings.NewReader(form.Encode())
	}
	request, err := http.NewRequest(method, target, body)
	if err != nil {
		t.Fatal(err)
	}
	if form != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func readCredentialBody(t *testing.T, response *http.Response) string {
	t.Helper()
	defer response.Body.Close()
	value, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(value)
}

// TestCredentialOriginServicePostgresIntegration is a no-SKIP CI path through
// the real HTTP handler, repository, PKCE exchange and signed JWKS verifier.
func TestCredentialOriginServicePostgresIntegration(t *testing.T) {
	databaseURL := tokenPaginationIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	repository := newTokenQueryIntegrationRepository(t, ctx, databaseURL)
	secrets, err := NewLocalAESProvider("credential-origin-key", []byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	repository.secrets = secrets
	now := time.Now().UTC().Truncate(time.Microsecond)
	repository.now = func() time.Time { return now }
	actor, owner, accountID, walletID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	authority := CredentialIsolationAuthoritySnapshot{ActorID: actor, Issuer: "placeholder", Subject: actor.String(), AuthorizationVersion: 17, GateEpoch: 23}
	repository.WithCredentialIsolationAuthorityGuard(credentialAuthorityGuardFunc(func(_ context.Context, _ pgx.Tx, observed CredentialIsolationAuthoritySnapshot) error {
		if observed != authority {
			return ErrTokenRouteActivationChanged
		}
		return nil
	}))
	if _, err = repository.pool.Exec(ctx, `INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode) VALUES($1,'USER',$2,$3,'Origin integration','ACTIVE',$4,$5,$5,'PLATFORM'); INSERT INTO token_share_policies(token_account_id,scope,updated_at) VALUES($1,'OWNER_UNIT_ONLY',$5)`, pgx.QueryExecModeSimpleProtocol, accountID, owner, walletID, actor, now); err != nil {
		t.Fatal(err)
	}
	idp := newControlledCredentialIDP(t, actor, "credential-client")
	authority.Issuer = idp.server.URL
	launchContext := withCredentialIsolationAuthority(ctx, authority)
	launch, err := repository.CreateCredentialLaunch(launchContext, accountID, actor, CredentialLaunchRequest{Action: "ENROLL", ExpectedAccountVersion: 1, Slot: "PRIMARY", Reason: "exercise the complete credential origin handler"}, "credential-origin-launch-key", "credential-origin-request")
	if err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	publicURL, _ := url.Parse("http://" + listener.Addr().String())
	issuerURL, _ := url.Parse(idp.server.URL)
	authorizationURL, _ := url.Parse(idp.server.URL + "/authorize")
	tokenURL, _ := url.Parse(idp.server.URL + "/token")
	jwksURL, _ := url.Parse(idp.server.URL + "/jwks")
	portalListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	portalURL, _ := url.Parse("http://" + portalListener.Addr().String())
	portalService, err := adminportal.NewService(adminportal.Config{
		PublicURL: portalURL, AuthorizationURL: authorizationURL, TokenURL: tokenURL,
		AccountURL: issuerURL, BillingURL: issuerURL, TokenGatewayURL: issuerURL,
		ClientID: "credential-real-browser", AdminTokenDetailsEnabled: true, CredentialOrigin: publicURL,
	}, &adminportal.RedisStore{})
	if err != nil {
		t.Fatal(err)
	}
	portalHTTP := &http.Server{Handler: portalService.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/start" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, `<!doctype html><html><head><meta charset="utf-8"><title>Credential handoff integration</title></head><body><main>Credential handoff integration</main></body></html>`)
	}))}
	go func() { _ = portalHTTP.Serve(portalListener) }()
	t.Cleanup(func() { _ = portalHTTP.Shutdown(context.Background()) })
	verify, err := NewCredentialOIDCVerifyFunc(ctx, idp.server.URL, jwksURL.String(), "credential-client", "test")
	if err != nil {
		t.Fatal(err)
	}
	providerPolicy, _ := NewProviderURLPolicy("test", []string{"provider.example"})
	service, err := NewCredentialOriginService(CredentialOriginConfig{PublicURL: publicURL, PortalOrigin: portalURL, IssuerURL: issuerURL, AuthorizationURL: authorizationURL, TokenURL: tokenURL, JWKSURL: jwksURL, ClientID: "credential-client", Verify: verify}, repository, acceptingCredentialValidator{}, providerPolicy)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: service.Handler(), ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	response := credentialHTTP(t, client, http.MethodPost, publicURL.String()+"/launch", url.Values{"launch_id": {launch.LaunchID.String()}}, portalURL.String())
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/session" {
		t.Fatalf("launch status=%d location=%s body=%s", response.StatusCode, response.Header.Get("Location"), readCredentialBody(t, response))
	}
	_ = response.Body.Close()
	response = credentialHTTP(t, client, http.MethodGet, publicURL.String()+"/session", nil, "")
	authorizationBody := readCredentialBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("session status=%d body=%s", response.StatusCode, authorizationBody)
	}
	authorizationQuery := url.Values{}
	for _, field := range []string{"client_id", "response_type", "redirect_uri", "scope", "state", "nonce", "code_challenge", "code_challenge_method", "prompt", "max_age"} {
		authorizationQuery.Set(field, credentialHiddenValue(t, authorizationBody, field))
	}
	// The first heartbeat is already durable; model a 20-second IdP journey.
	now = now.Add(20 * time.Second)
	response = credentialHTTP(t, client, http.MethodGet, authorizationURL.String()+"?"+authorizationQuery.Encode(), nil, "")
	callbackLocation := response.Header.Get("Location")
	_ = response.Body.Close()
	if response.StatusCode != http.StatusSeeOther || !strings.HasPrefix(callbackLocation, publicURL.String()+"/oidc/callback") {
		t.Fatalf("authorization status=%d location=%s", response.StatusCode, callbackLocation)
	}
	wrongState, _ := url.Parse(callbackLocation)
	wrongQuery := wrongState.Query()
	wrongQuery.Set("state", "wrong-state")
	wrongState.RawQuery = wrongQuery.Encode()
	response = credentialHTTP(t, client, http.MethodGet, wrongState.String(), nil, "")
	if response.StatusCode != http.StatusBadRequest || idp.tokenHits.Load() != 0 {
		t.Fatalf("wrong state status=%d token_hits=%d body=%s", response.StatusCode, idp.tokenHits.Load(), readCredentialBody(t, response))
	}
	_ = response.Body.Close()
	response = credentialHTTP(t, client, http.MethodGet, callbackLocation, nil, "")
	callbackBody := readCredentialBody(t, response)
	if response.StatusCode != http.StatusOK || idp.tokenHits.Load() != 1 || !strings.Contains(callbackBody, "pinvou-credential-next") {
		t.Fatalf("callback status=%d token_hits=%d body=%s", response.StatusCode, idp.tokenHits.Load(), callbackBody)
	}
	response = credentialHTTP(t, client, http.MethodGet, publicURL.String()+"/session", nil, "")
	credentialBody := readCredentialBody(t, response)
	csrf := credentialHiddenValue(t, credentialBody, "csrf")
	response = credentialHTTP(t, client, http.MethodPost, publicURL.String()+"/credentials", url.Values{"csrf": {csrf}, "provider": {"NEW_API"}, "base_url": {"https://provider.example"}, "allowed_models": {"model-a"}, "secret": {"origin-integration-secret"}}, publicURL.String())
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/session" {
		t.Fatalf("credential submit status=%d location=%s body=%s", response.StatusCode, response.Header.Get("Location"), readCredentialBody(t, response))
	}
	_ = response.Body.Close()
	response = credentialHTTP(t, client, http.MethodGet, publicURL.String()+"/session", nil, "")
	terminalBody := readCredentialBody(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(terminalBody, "Credential updated securely") {
		t.Fatalf("terminal refresh status=%d body=%s", response.StatusCode, terminalBody)
	}
	var credentialCount int
	if err = repository.pool.QueryRow(ctx, `SELECT count(*) FROM provider_credentials WHERE token_account_id=$1 AND status='ACTIVE'`, accountID).Scan(&credentialCount); err != nil || credentialCount != 1 {
		t.Fatalf("credential count=%d err=%v", credentialCount, err)
	}
	if os.Getenv("PINVOU_REQUIRE_CREDENTIAL_BROWSER") == "1" {
		launchIDs := make([]string, 0, 5)
		browserAccounts := make([]uuid.UUID, 0, 5)
		for index := 0; index < 5; index++ {
			browserAccount, browserWallet := uuid.New(), uuid.New()
			if _, err = repository.pool.Exec(ctx, `INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode) VALUES($1,'USER',$2,$3,$4,'ACTIVE',$5,$6,$6,'PLATFORM'); INSERT INTO token_share_policies(token_account_id,scope,updated_at) VALUES($1,'OWNER_UNIT_ONLY',$6)`, pgx.QueryExecModeSimpleProtocol, browserAccount, owner, browserWallet, fmt.Sprintf("Browser credential %d", index), actor, now); err != nil {
				t.Fatal(err)
			}
			browserLaunch, launchErr := repository.CreateCredentialLaunch(launchContext, browserAccount, actor, CredentialLaunchRequest{Action: "ENROLL", ExpectedAccountVersion: 1, Slot: "PRIMARY", Reason: "exercise the real credential origin in Chromium"}, fmt.Sprintf("browser-credential-launch-key-%d", index), fmt.Sprintf("browser-credential-request-%d", index))
			if launchErr != nil {
				t.Fatal(launchErr)
			}
			launchIDs = append(launchIDs, browserLaunch.LaunchID.String())
			browserAccounts = append(browserAccounts, browserAccount)
		}
		appDirectory, _ := filepath.Abs(filepath.Join("..", "..", "..", "pinvou3-app"))
		script := filepath.Join(appDirectory, "tests", "admin_token_credential_origin_real_smoke.mjs")
		arguments := append([]string{script, portalURL.String()}, launchIDs...)
		command := exec.CommandContext(ctx, "node", arguments...)
		command.Dir = appDirectory
		command.Env = os.Environ()
		browserOutput, browserErr := command.CombinedOutput()
		if browserErr != nil {
			t.Fatalf("real credential Chromium smoke failed: %v\n%s", browserErr, browserOutput)
		}
		if !strings.Contains(string(browserOutput), "5 consecutive flows") {
			t.Fatalf("real credential Chromium smoke did not report completion: %s", browserOutput)
		}
		for _, browserAccount := range browserAccounts {
			if err = repository.pool.QueryRow(ctx, `SELECT count(*) FROM provider_credentials WHERE token_account_id=$1 AND status='ACTIVE'`, browserAccount).Scan(&credentialCount); err != nil || credentialCount != 1 {
				t.Fatalf("browser credential account=%s count=%d err=%v", browserAccount, credentialCount, err)
			}
		}
	}

	// A Portal revocation that wins before the callback prevents even the IdP
	// exchange, and therefore cannot reach credential persistence.
	revokedAccount, revokedWallet := uuid.New(), uuid.New()
	if _, err = repository.pool.Exec(ctx, `INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode) VALUES($1,'USER',$2,$3,'Revoked origin race','ACTIVE',$4,$5,$5,'PLATFORM'); INSERT INTO token_share_policies(token_account_id,scope,updated_at) VALUES($1,'OWNER_UNIT_ONLY',$5)`, pgx.QueryExecModeSimpleProtocol, revokedAccount, owner, revokedWallet, actor, now); err != nil {
		t.Fatal(err)
	}
	revokedLaunch, err := repository.CreateCredentialLaunch(launchContext, revokedAccount, actor, CredentialLaunchRequest{Action: "ENROLL", ExpectedAccountVersion: 1, Slot: "PRIMARY", Reason: "revoke before credential callback exchange"}, "credential-origin-revoked-key", "credential-origin-revoked-request")
	if err != nil {
		t.Fatal(err)
	}
	revokedJar, _ := cookiejar.New(nil)
	revokedClient := &http.Client{Jar: revokedJar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response = credentialHTTP(t, revokedClient, http.MethodPost, publicURL.String()+"/launch", url.Values{"launch_id": {revokedLaunch.LaunchID.String()}}, portalURL.String())
	_ = response.Body.Close()
	response = credentialHTTP(t, revokedClient, http.MethodGet, publicURL.String()+"/session", nil, "")
	revokedAuthorizationBody := readCredentialBody(t, response)
	revokedQuery := url.Values{}
	for _, field := range []string{"client_id", "response_type", "redirect_uri", "scope", "state", "nonce", "code_challenge", "code_challenge_method", "prompt", "max_age"} {
		revokedQuery.Set(field, credentialHiddenValue(t, revokedAuthorizationBody, field))
	}
	response = credentialHTTP(t, revokedClient, http.MethodGet, authorizationURL.String()+"?"+revokedQuery.Encode(), nil, "")
	revokedCallback := response.Header.Get("Location")
	_ = response.Body.Close()
	if err = repository.RevokeCredentialLaunch(ctx, actor, revokedLaunch.ResultRef, "CREDENTIAL_PERMISSION_REVOKED"); err != nil {
		t.Fatal(err)
	}
	expectedTokenHits := int64(1)
	if os.Getenv("PINVOU_REQUIRE_CREDENTIAL_BROWSER") == "1" {
		expectedTokenHits += 5
	}
	if idp.tokenHits.Load() != expectedTokenHits {
		t.Fatalf("successful flows made %d token exchanges, expected %d", idp.tokenHits.Load(), expectedTokenHits)
	}
	beforeRejectedCallback := credentialOriginPersistedFacts(t, ctx, repository)
	response = credentialHTTP(t, revokedClient, http.MethodGet, revokedCallback, nil, "")
	var rejected struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(readCredentialBody(t, response)), &rejected); err != nil || response.StatusCode != http.StatusUnauthorized || rejected.Code != "CREDENTIAL_SESSION_INVALID" {
		t.Fatalf("revoked callback status=%d code=%s decode_error=%v", response.StatusCode, rejected.Code, err)
	}
	if idp.tokenHits.Load() != expectedTokenHits {
		t.Fatalf("revoked flow reached IdP token exchange: %d", idp.tokenHits.Load())
	}
	if credentialOriginPersistedFacts(t, ctx, repository) != beforeRejectedCallback {
		t.Fatal("rejected callback changed persistent credential/domain facts")
	}
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM provider_credentials WHERE token_account_id=$1`, revokedAccount).Scan(&credentialCount); err != nil || credentialCount != 0 {
		t.Fatalf("revoked account credential count=%d err=%v", credentialCount, err)
	}
	for _, secret := range []string{"origin-integration-secret", "code_verifier", "client_secret"} {
		if strings.Contains(authorizationBody+callbackBody+credentialBody+terminalBody, secret) {
			t.Fatalf("credential flow response leaked %q", secret)
		}
	}
}

func credentialOriginPersistedFacts(t *testing.T, ctx context.Context, repository *PostgresRepository) string {
	t.Helper()
	var facts string
	if err := repository.pool.QueryRow(ctx, `SELECT jsonb_build_object(
'accounts',(SELECT jsonb_agg(to_jsonb(a) ORDER BY a.id) FROM token_accounts a),
'credentials',(SELECT jsonb_agg(to_jsonb(c) ORDER BY c.id) FROM provider_credentials c),
'launches',(SELECT jsonb_agg(to_jsonb(l) ORDER BY l.id) FROM credential_launches l),
'sessions',(SELECT jsonb_agg(to_jsonb(s) ORDER BY s.id) FROM credential_isolation_sessions s),
'results',(SELECT jsonb_agg(to_jsonb(r) ORDER BY r.result_ref) FROM credential_isolation_results r),
'audits',(SELECT jsonb_agg(to_jsonb(a) ORDER BY a.id) FROM token_audit_logs a),
'outbox',(SELECT jsonb_agg(to_jsonb(o) ORDER BY o.event_id) FROM outbox_events o))::text`).Scan(&facts); err != nil {
		t.Fatal(err)
	}
	return facts
}
