package portal

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type memoryStore struct {
	mu       sync.Mutex
	flows    map[string]LoginFlow
	sessions map[string]Session
}

func newMemoryStore() *memoryStore {
	return &memoryStore{flows: map[string]LoginFlow{}, sessions: map[string]Session{}}
}
func (s *memoryStore) SaveFlow(_ context.Context, key string, value LoginFlow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flows[key] = value
	return nil
}
func (s *memoryStore) TakeFlow(_ context.Context, key string) (LoginFlow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.flows[key]
	delete(s.flows, key)
	if !ok {
		return LoginFlow{}, ErrSessionNotFound
	}
	return value, nil
}
func (s *memoryStore) SaveSession(_ context.Context, value Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[value.ID] = value
	return nil
}
func (s *memoryStore) GetSession(_ context.Context, key string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.sessions[key]
	if !ok {
		return Session{}, ErrSessionNotFound
	}
	return value, nil
}
func (s *memoryStore) DeleteSession(_ context.Context, key string) error {
	delete(s.sessions, key)
	return nil
}
func (*memoryStore) TryRefreshLock(context.Context, string) (bool, error) { return true, nil }
func (*memoryStore) ReleaseRefreshLock(context.Context, string) error     { return nil }
func (*memoryStore) Ping(context.Context) error                           { return nil }
func (*memoryStore) Close() error                                         { return nil }

func testService(t *testing.T, store Store) *Service {
	t.Helper()
	parse := func(value string) *url.URL {
		parsed, err := url.Parse(value)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	service, err := NewService(Config{PublicURL: parse("https://portal.example"), AuthorizationURL: parse("https://identity.example/auth"), TokenURL: parse("https://identity.internal/token"), ClientID: "portal", AccountURL: parse("https://account.internal"), BillingURL: parse("https://billing.internal"), TokenGatewayURL: parse("https://token.internal"), SecureCookies: true}, store)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestLoginCreatesServerSidePKCEFlow(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/auth/login?return_to=/wallet", nil))
	if response.Code != http.StatusFound {
		t.Fatalf("status = %d", response.Code)
	}
	location, _ := url.Parse(response.Header().Get("Location"))
	if location.Host != "identity.example" || location.Query().Get("code_challenge_method") != "S256" || location.Query().Get("code_challenge") == "" {
		t.Fatalf("unexpected authorization redirect: %s", location)
	}
	state := location.Query().Get("state")
	flow := store.flows[state]
	if flow.Verifier == "" || flow.ReturnTo != "/wallet" {
		t.Fatalf("unexpected stored flow: %+v", flow)
	}
	if !strings.Contains(response.Header().Get("Set-Cookie"), "__Host-portal_flow=") || strings.Contains(response.Header().Get("Set-Cookie"), flow.Verifier) {
		t.Fatal("flow cookie must contain only the opaque state")
	}
}

func TestMutationRequiresExactOriginAndCSRF(t *testing.T) {
	service := testService(t, newMemoryStore())
	session := Session{CSRFToken: strings.Repeat("a", 32)}
	request := httptest.NewRequest(http.MethodPost, "https://portal.example/api/logout", nil)
	request.Header.Set("Origin", "https://portal.example")
	request.Header.Set("X-CSRF-Token", session.CSRFToken)
	request.AddCookie(&http.Cookie{Name: service.csrfCookieName(), Value: session.CSRFToken})
	if !service.validMutation(request, session) {
		t.Fatal("exact-origin request with matching token should pass")
	}
	request.Header.Set("Origin", "https://evil.example")
	if service.validMutation(request, session) {
		t.Fatal("cross-origin request must be rejected")
	}
}

func TestSessionEndpointDoesNotExposeOIDCTokens(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	session := Session{ID: "session", AccessToken: "access-secret", RefreshToken: "refresh-secret", AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(2 * time.Hour), CSRFToken: strings.Repeat("c", 32)}
	_ = store.SaveSession(context.Background(), session)
	request := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)
	body := response.Body.String()
	if response.Code != http.StatusOK || strings.Contains(body, "access-secret") || strings.Contains(body, "refresh-secret") || !strings.Contains(body, session.CSRFToken) {
		t.Fatalf("unexpected session response: %d %s", response.Code, body)
	}
}

func TestCallbackAcceptsNonExpiringOfflineRefreshTokenWithBoundedSession(t *testing.T) {
	store := newMemoryStore()
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code_verifier") != "verifier" {
			t.Fatalf("unexpected token request: %v", r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"access","refresh_token":"refresh","token_type":"Bearer","expires_in":300,"refresh_expires_in":0}`)
	}))
	defer tokenServer.Close()

	parse := func(raw string) *url.URL {
		parsed, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	service, err := NewService(Config{
		PublicURL:        parse("https://portal.example"),
		AuthorizationURL: parse("https://identity.example/auth"),
		TokenURL:         parse(tokenServer.URL),
		ClientID:         "portal",
		AccountURL:       parse("https://account.internal"),
		BillingURL:       parse("https://billing.internal"),
		TokenGatewayURL:  parse("https://token.internal"),
		SecureCookies:    true,
	}, store)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 13, 0, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	store.flows["state"] = LoginFlow{Verifier: "verifier", ExpiresAt: now.Add(time.Minute), ReturnTo: "/wallet"}

	request := httptest.NewRequest(http.MethodGet, "/auth/callback?state=state&code=code", nil)
	request.AddCookie(&http.Cookie{Name: "__Host-portal_flow", Value: "state"})
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)

	if response.Code != http.StatusFound || response.Header().Get("Location") != "/wallet" {
		t.Fatalf("callback response = %d %s", response.Code, response.Body.String())
	}
	if len(store.sessions) != 1 {
		t.Fatalf("saved sessions = %d", len(store.sessions))
	}
	for _, session := range store.sessions {
		if got, want := session.RefreshExpiresAt, now.Add(defaultSessionMaxAge); !got.Equal(want) {
			t.Fatalf("refresh expiry = %s, want %s", got, want)
		}
	}
}

func TestPortalProxyExcludesModelDataPlane(t *testing.T) {
	if tokenPathAllowed(http.MethodPost, "/v1/chat/completions") {
		t.Fatal("Portal BFF must not expose the model data plane")
	}
	if !tokenPathAllowed(http.MethodGet, "/v1/me/balance") || !accountPathAllowed(http.MethodGet, "/v1/me") {
		t.Fatal("Portal BFF must expose the documented management APIs")
	}
	if safeReturnTo("//evil.example") != "/" || safeReturnTo("https://evil.example") != "/" {
		t.Fatal("external post-login redirects must be rejected")
	}
}

func TestAdminProxyAllowsOnlyDocumentedManagementEndpoints(t *testing.T) {
	if !accountAdminPathAllowed(http.MethodGet, "/admin/v1/users") || !billingAdminPathAllowed(http.MethodGet, "/admin/v1/wallets") || !tokenAdminPathAllowed(http.MethodGet, "/admin/v1/token-accounts") {
		t.Fatal("admin portal must expose the documented management APIs")
	}
	userID := "0198fbf7-c3ba-7bc4-8849-c157740963de"
	organizationID := "0198fbf7-c3ba-7bc4-8849-c157740963df"
	unitID := "0198fbf7-c3ba-7bc4-8849-c157740963e0"
	if !accountAdminPathAllowed(http.MethodPatch, "/admin/v1/users/"+userID+"/status") || !accountAdminPathAllowed(http.MethodDelete, "/admin/v1/users/"+userID) || !accountAdminPathAllowed(http.MethodPost, "/admin/v1/organizations") || !accountAdminPathAllowed(http.MethodPatch, "/admin/v1/organizations/"+organizationID) || !accountAdminPathAllowed(http.MethodPatch, "/admin/v1/organizations/"+organizationID+"/status") || !accountAdminPathAllowed(http.MethodDelete, "/admin/v1/organizations/"+organizationID) || !accountAdminPathAllowed(http.MethodGet, "/admin/v1/organizations/"+organizationID+"/members") || !accountAdminPathAllowed(http.MethodPost, "/admin/v1/organizations/"+organizationID+"/ownership") || !accountAdminPathAllowed(http.MethodGet, "/admin/v1/organizations/"+organizationID+"/units") || !accountAdminPathAllowed(http.MethodPost, "/admin/v1/organizations/"+organizationID+"/units") || !accountAdminPathAllowed(http.MethodPatch, "/admin/v1/organizations/"+organizationID+"/units/"+unitID) || !accountAdminPathAllowed(http.MethodDelete, "/admin/v1/organizations/"+organizationID+"/units/"+unitID) {
		t.Fatal("admin portal must expose account lifecycle mutations")
	}
	if accountAdminPathAllowed(http.MethodPost, "/admin/v1/users") || accountAdminPathAllowed(http.MethodDelete, "/admin/v1/users/not-a-uuid") || accountAdminPathAllowed(http.MethodPost, "/admin/v1/organizations/"+organizationID+"/members") || accountAdminPathAllowed(http.MethodGet, "/admin/v1/organizations/"+organizationID+"/ownership") || accountAdminPathAllowed(http.MethodDelete, "/admin/v1/organizations/"+organizationID+"/units/not-a-uuid") || billingAdminPathAllowed(http.MethodGet, "/internal/wallets") || tokenAdminPathAllowed(http.MethodGet, "/v1/chat/completions") {
		t.Fatal("admin portal must not expose undocumented mutations, internal APIs, or the model data plane")
	}
}

func TestSecurePortalSetsIsolationHeaders(t *testing.T) {
	response := httptest.NewRecorder()
	testService(t, newMemoryStore()).Handler(nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	for header, expected := range map[string]string{
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Strict-Transport-Security":    "max-age=31536000; includeSubDomains",
		"X-Frame-Options":              "DENY",
	} {
		if response.Header().Get(header) != expected {
			t.Fatalf("%s = %q, want %q", header, response.Header().Get(header), expected)
		}
	}
}
