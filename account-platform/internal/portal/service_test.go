package portal

import (
	"context"
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
	service, err := NewService(Config{PublicURL: parse("https://portal.example"), AuthorizationURL: parse("https://identity.example/auth"), TokenURL: parse("https://identity.internal/token"), ClientID: "portal", AccountURL: parse("https://account.internal"), TokenGatewayURL: parse("https://token.internal"), SecureCookies: true}, store)
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
