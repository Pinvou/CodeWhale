package portal

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/requestdeadline"
	"github.com/google/uuid"
)

type memoryStore struct {
	mu                      sync.Mutex
	flows                   map[string]LoginFlow
	sessions                map[string]Session
	approvalClassifications map[string]ApprovalClassification
	approvalWires           map[string]int
}

type unavailableSessionStore struct{ *memoryStore }

func (s *unavailableSessionStore) GetSession(context.Context, string) (Session, error) {
	return Session{}, errors.New("session store unavailable")
}

func newMemoryStore() *memoryStore {
	return &memoryStore{flows: map[string]LoginFlow{}, sessions: map[string]Session{}, approvalClassifications: map[string]ApprovalClassification{}, approvalWires: map[string]int{}}
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
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, key)
	delete(s.approvalWires, key)
	return nil
}

func (s *memoryStore) LockApprovalWireVersion(_ context.Context, session Session, preferred int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if preferred != 1 && preferred != 2 {
		return 0, ErrApprovalWireUnavailable
	}
	if selected, ok := s.approvalWires[session.ID]; ok {
		return selected, nil
	}
	if !session.ApprovalV2Eligible {
		preferred = 1
	}
	s.approvalWires[session.ID] = preferred
	return preferred, nil
}
func (s *memoryStore) SaveApprovalClassification(_ context.Context, proof ApprovalClassification) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.approvalClassifications[approvalClassificationKey(proof.SessionID, proof.ApprovalRequestID)] = proof
	return nil
}
func (s *memoryStore) GetApprovalClassification(_ context.Context, sessionID, approvalRequestID string) (ApprovalClassification, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	proof, ok := s.approvalClassifications[approvalClassificationKey(sessionID, approvalRequestID)]
	if !ok {
		return ApprovalClassification{}, ErrSessionNotFound
	}
	return proof, nil
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

func TestAnonymousPortalRootStartsIdentityLoginImmediately(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	staticCalled := false
	static := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { staticCalled = true })
	response := httptest.NewRecorder()

	service.Handler(static).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/?section=orders", nil))

	if response.Code != http.StatusFound {
		t.Fatalf("status = %d", response.Code)
	}
	location, _ := url.Parse(response.Header().Get("Location"))
	if location.Host != "identity.example" || location.Query().Get("code_challenge_method") != "S256" {
		t.Fatalf("anonymous root must start the identity flow directly: %s", location)
	}
	flow := store.flows[location.Query().Get("state")]
	if flow.ReturnTo != "/?section=orders" {
		t.Fatalf("unexpected return target: %q", flow.ReturnTo)
	}
	if staticCalled {
		t.Fatal("anonymous root must not render the portal shell before login")
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("anonymous root must not cache the removed landing page: %q", response.Header().Get("Cache-Control"))
	}
}

func TestAnonymousPortalRootReturnsToCreditsAndUsageByDefault(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	response := httptest.NewRecorder()

	service.Handler(http.NotFoundHandler()).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))

	if response.Code != http.StatusFound {
		t.Fatalf("status = %d", response.Code)
	}
	location, _ := url.Parse(response.Header().Get("Location"))
	flow := store.flows[location.Query().Get("state")]
	if flow.ReturnTo != "/?section=creditsUsage" {
		t.Fatalf("default return target = %q", flow.ReturnTo)
	}
}

func TestPortalDoesNotTurnSessionStoreFailureIntoLoginRedirect(t *testing.T) {
	store := &unavailableSessionStore{memoryStore: newMemoryStore()}
	service := testService(t, store)
	request := httptest.NewRequest(http.MethodGet, "/?section=creditsUsage", nil)
	request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: "existing-session"})
	response := httptest.NewRecorder()

	service.Handler(http.NotFoundHandler()).ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", response.Code)
	}
	if location := response.Header().Get("Location"); location != "" {
		t.Fatalf("session store failure must not start a login loop: %q", location)
	}
}

func TestSessionEndpointDistinguishesStoreFailureFromExpiredLogin(t *testing.T) {
	store := &unavailableSessionStore{memoryStore: newMemoryStore()}
	service := testService(t, store)
	request := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: "existing-session"})
	response := httptest.NewRecorder()

	service.Handler(nil).ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "SESSION_UNAVAILABLE") {
		t.Fatalf("unexpected session response: %d %s", response.Code, response.Body.String())
	}
}

func TestAuthenticatedPortalRootRedirectsToCreditsAndUsageByDefault(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	now := time.Now().UTC()
	session := Session{ID: "portal-session", AccessToken: "access", RefreshToken: "refresh", AccessExpiresAt: now.Add(time.Hour), RefreshExpiresAt: now.Add(2 * time.Hour), CSRFToken: strings.Repeat("p", 32)}
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
	response := httptest.NewRecorder()
	static := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "portal application")
	})

	service.Handler(static).ServeHTTP(response, request)

	if response.Code != http.StatusFound || response.Header().Get("Location") != "/?section=creditsUsage" {
		t.Fatalf("authenticated root must select the first tab: %d %q", response.Code, response.Header().Get("Location"))
	}
	if response.Body.String() == "portal application" {
		t.Fatal("the portal shell must not render before the default section is canonicalized")
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("authenticated root must not persist a stale portal shell: %q", response.Header().Get("Cache-Control"))
	}
}

func TestAuthenticatedPortalRootPreservesExplicitSection(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	now := time.Now().UTC()
	session := Session{ID: "portal-session", AccessToken: "access", RefreshToken: "refresh", AccessExpiresAt: now.Add(time.Hour), RefreshExpiresAt: now.Add(2 * time.Hour), CSRFToken: strings.Repeat("p", 32)}
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/?section=accountSettings", nil)
	request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
	response := httptest.NewRecorder()
	static := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "portal application")
	})

	service.Handler(static).ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Body.String() != "portal application" {
		t.Fatalf("explicit section must serve the portal unchanged: %d %q", response.Code, response.Body.String())
	}
}

func TestReauthenticationForcesIdentityLoginAndTracksPreviousSession(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	now := time.Now().UTC()
	session := Session{ID: "existing-session", AccessToken: "access", RefreshToken: "refresh", AccessExpiresAt: now.Add(time.Hour), RefreshExpiresAt: now.Add(2 * time.Hour), CSRFToken: strings.Repeat("r", 32)}
	_ = store.SaveSession(context.Background(), session)
	request := httptest.NewRequest(http.MethodGet, "/auth/login?reauthenticate=1&return_to=%2F%3Fsection%3Dprofile", nil)
	request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)
	if response.Code != http.StatusFound {
		t.Fatalf("status = %d", response.Code)
	}
	location, _ := url.Parse(response.Header().Get("Location"))
	if location.Query().Get("prompt") != "login" || location.Query().Get("max_age") != "900" {
		t.Fatalf("reauthentication must force a fresh identity login: %s", location)
	}
	flow := store.flows[location.Query().Get("state")]
	if flow.PreviousSession != session.ID || flow.ReturnTo != "/?section=profile" {
		t.Fatalf("unexpected reauthentication flow: %+v", flow)
	}
}

func TestReauthenticationRequiresExistingPortalSession(t *testing.T) {
	service := testService(t, newMemoryStore())
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/auth/login?reauthenticate=1", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", response.Code)
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

func TestTokenAdminIngressDiscardsForgedDeadlineAndClassifiesCursor(t *testing.T) {
	service := testService(t, newMemoryStore())
	request := httptest.NewRequest(http.MethodGet, "/api/admin/token/admin/v1/models?cursor=signed", nil)
	request.Header.Set(requestdeadline.AbsoluteDeadlineHeader, time.Now().Add(time.Hour).Format(time.RFC3339Nano))
	request.Header.Set(requestdeadline.DeadlineClassHeader, string(requestdeadline.SnapshotBuild))
	response := httptest.NewRecorder()
	called := false
	service.tokenAdminIngressDeadline(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
		contract, ok := requestdeadline.FromContext(r.Context())
		if !ok || contract.Class != requestdeadline.SnapshotRead {
			t.Fatalf("trusted ingress contract = %+v, %v", contract, ok)
		}
		if remaining := time.Until(contract.IngressDeadline); remaining <= 6*time.Second || remaining > 7*time.Second {
			t.Fatalf("read ingress budget was reset or enlarged: %s", remaining)
		}
		if r.Header.Get(requestdeadline.AbsoluteDeadlineHeader) != "" || r.Header.Get(requestdeadline.DeadlineClassHeader) != "" {
			t.Fatal("client-controlled deadline headers reached the application")
		}
	})).ServeHTTP(response, request)
	if !called {
		t.Fatal("deadline middleware did not call the application")
	}
}

func TestTokenAdminIngressClassifiesExactOverviewBeforeSnapshotBuild(t *testing.T) {
	service := testService(t, newMemoryStore())
	request := httptest.NewRequest(http.MethodGet, "/api/admin/token/admin/v1/overviews/model-catalog", nil)
	request.Header.Set(requestdeadline.AbsoluteDeadlineHeader, time.Now().Add(time.Hour).Format(time.RFC3339Nano))
	request.Header.Set(requestdeadline.DeadlineClassHeader, string(requestdeadline.SnapshotBuild))
	response := httptest.NewRecorder()
	service.tokenAdminIngressDeadline(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		contract, ok := requestdeadline.FromContext(r.Context())
		if !ok || contract.Class != requestdeadline.OverviewRead {
			t.Fatalf("trusted ingress contract = %+v, %v", contract, ok)
		}
		if remaining := time.Until(contract.BusinessDeadline); remaining <= 2*time.Second || remaining > 3*time.Second {
			t.Fatalf("overview business budget was reset or enlarged: %s", remaining)
		}
		if r.Header.Get(requestdeadline.AbsoluteDeadlineHeader) != "" || r.Header.Get(requestdeadline.DeadlineClassHeader) != "" {
			t.Fatal("client-controlled overview deadline headers reached the application")
		}
	})).ServeHTTP(response, request)
}

func TestTokenPageOverviewProxyFailsClosedWithoutStrongHMACKey(t *testing.T) {
	for _, key := range [][]byte{nil, []byte(strings.Repeat("k", 31))} {
		for _, item := range []struct {
			path, code string
		}{
			{"/api/admin/token/admin/v1/overviews/model-catalog", "MODEL_CATALOG_OVERVIEW_UNAVAILABLE"},
			{"/api/admin/token/admin/v1/overviews/token-resources", "TOKEN_RESOURCES_OVERVIEW_UNAVAILABLE"},
			{"/api/admin/token/admin/v1/overviews/model-operations?window=24h", "MODEL_OPERATIONS_OVERVIEW_UNAVAILABLE"},
		} {
			service := testService(t, newMemoryStore())
			service.config.TokenAdminProxyHMACKey = key
			request := httptest.NewRequest(http.MethodGet, item.path, nil)
			response := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(response, request)
			if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), item.code) {
				t.Fatalf("key_length=%d path=%s status=%d body=%s", len(key), item.path, response.Code, response.Body.String())
			}
		}
	}
}

func TestTokenPageOverviewProxyRejectsEveryNonCanonicalAliasBeforeUpstream(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	service.config.TokenAdminProxyHMACKey = []byte(strings.Repeat("c", 32))
	upstreamHits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	service.config.TokenGatewayURL = upstreamURL
	session := Session{ID: "overview-paths", AccessToken: "access", RefreshToken: "refresh", AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(2 * time.Hour), CSRFToken: strings.Repeat("c", 32)}
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	handler := service.Handler(nil)
	for _, exact := range []string{
		"/api/admin/token/admin/v1/overviews/model-catalog",
		"/api/admin/token/admin/v1/overviews/token-resources",
		"/api/admin/token/admin/v1/overviews/model-operations?window=24h",
	} {
		request := httptest.NewRequest(http.MethodGet, exact, nil)
		request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("canonical path %q status=%d body=%s", exact, response.Code, response.Body.String())
		}
	}
	if upstreamHits != 3 {
		t.Fatalf("canonical paths reached upstream %d times, want 3", upstreamHits)
	}

	aliases := []string{
		"/api/admin/token/admin/v1/overviews/model-catalog/",
		"/api/admin/token/admin/v1/overviews/token-resources/",
		"/api/admin/token/admin/v1/overviews/model-operations/",
		"/api/admin/token/admin/v1/overviews//model-catalog",
		"/api/admin/token/admin//v1/overviews/token-resources",
		"/api/admin/token/admin/v1/overviews/./model-operations",
		"/api/admin/token/admin/v1/overviews/ignored/../model-catalog",
		"/api/admin/token/admin/v1/overviews/%6dodel-catalog",
		"/api/admin/token/admin/v1/overviews/%2e/token-resources",
		"/api/admin/token/admin/v1/overviews%2fmodel-operations",
	}
	for _, alias := range aliases {
		request := httptest.NewRequest(http.MethodGet, alias, nil)
		request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "NOT_FOUND") {
			t.Fatalf("alias %q status=%d location=%q body=%s", alias, response.Code, response.Header().Get("Location"), response.Body.String())
		}
	}
	if upstreamHits != 3 {
		t.Fatalf("non-canonical aliases reached upstream: hits=%d", upstreamHits)
	}
}

func TestTokenAdminProxyForwardsOneAbsoluteBusinessDeadline(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(requestdeadline.DeadlineClassHeader) != string(requestdeadline.SnapshotBuild) {
			t.Fatalf("deadline class = %q", r.Header.Get(requestdeadline.DeadlineClassHeader))
		}
		deadline, err := time.Parse(time.RFC3339Nano, r.Header.Get(requestdeadline.AbsoluteDeadlineHeader))
		if err != nil {
			t.Fatalf("absolute deadline: %v", err)
		}
		if remaining := time.Until(deadline); remaining <= 9*time.Second || remaining > 10*time.Second {
			t.Fatalf("business budget was reset or enlarged: %s", remaining)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[]}`)
	}))
	defer upstream.Close()
	parsed, _ := url.Parse(upstream.URL)
	store := newMemoryStore()
	service := testService(t, store)
	service.config.TokenGatewayURL = parsed
	session := Session{ID: "deadline-session", AccessToken: "access", RefreshToken: "refresh", AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(2 * time.Hour), CSRFToken: strings.Repeat("d", 32)}
	_ = store.SaveSession(context.Background(), session)
	request := httptest.NewRequest(http.MethodGet, "/api/admin/token/admin/v1/models", nil)
	request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("proxy status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestTokenAdminProxyPreservesStableErrorsBeforeOuterDeadline(t *testing.T) {
	for _, item := range []struct {
		name, path, resource, code string
		class                      requestdeadline.Class
		status                     int
	}{
		{"build", "/api/admin/token/admin/v1/models", "/admin/v1/models", "PAGINATION_SNAPSHOT_UNAVAILABLE", requestdeadline.SnapshotBuild, 503},
		{"read", "/api/admin/token/admin/v1/models?cursor=signed", "/admin/v1/models", "QUERY_TIMEOUT", requestdeadline.SnapshotRead, 504},
		{"precheck", "/api/admin/token/admin/v1/models/impact", "/admin/v1/models/impact", "PRECHECK_TIMEOUT", requestdeadline.Precheck, 504},
		{"model-catalog-overview", "/api/admin/token/admin/v1/overviews/model-catalog", "/admin/v1/overviews/model-catalog", "MODEL_CATALOG_OVERVIEW_TIMEOUT", requestdeadline.OverviewRead, 504},
		{"token-resources-overview", "/api/admin/token/admin/v1/overviews/token-resources", "/admin/v1/overviews/token-resources", "TOKEN_RESOURCES_OVERVIEW_TIMEOUT", requestdeadline.OverviewRead, 504},
		{"model-operations-overview", "/api/admin/token/admin/v1/overviews/model-operations?window=24h", "/admin/v1/overviews/model-operations", "MODEL_OPERATIONS_OVERVIEW_TIMEOUT", requestdeadline.OverviewRead, 504},
	} {
		t.Run(item.name, func(t *testing.T) {
			observedDeadline := make(chan time.Time, 1)
			tokenHandler := requestdeadline.TokenAdminMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				deadline, ok := r.Context().Deadline()
				if !ok {
					t.Error("Token handler did not receive a business deadline")
					return
				}
				observedDeadline <- deadline
				<-r.Context().Done()
				if !errors.Is(r.Context().Err(), context.DeadlineExceeded) || !requestdeadline.WriteContextDeadlineError(w, r) {
					t.Errorf("Token handler did not classify its consumed deadline: %v", r.Context().Err())
				}
			}), time.Now)
			upstream := httptest.NewServer(tokenHandler)
			defer upstream.Close()
			parsed, _ := url.Parse(upstream.URL)
			store := newMemoryStore()
			service := testService(t, store)
			service.config.TokenGatewayURL = parsed
			budget, _ := requestdeadline.BudgetFor(item.class)
			portalStartedAt := time.Now().UTC().Add(-budget.Business + 250*time.Millisecond)
			businessDeadline := portalStartedAt.Add(budget.Business)
			service.deadlineNow = func() time.Time { return portalStartedAt }
			session := Session{ID: "deadline-" + item.name, AccessToken: "access", RefreshToken: "refresh", AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(2 * time.Hour), CSRFToken: strings.Repeat("e", 32)}
			_ = store.SaveSession(context.Background(), session)
			request := httptest.NewRequest(http.MethodGet, item.path, nil)
			request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
			request.Header.Set(requestdeadline.AbsoluteDeadlineHeader, time.Now().Add(time.Hour).Format(time.RFC3339Nano))
			request.Header.Set(requestdeadline.DeadlineClassHeader, string(requestdeadline.SnapshotBuild))
			response := httptest.NewRecorder()
			started := time.Now()
			application := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				service.proxy(w, r, service.config.TokenGatewayURL, item.resource, func(string, string) bool { return true })
			})
			service.tokenAdminIngressDeadline(application).ServeHTTP(response, request)
			var deadline time.Time
			select {
			case deadline = <-observedDeadline:
			case <-time.After(time.Second):
				t.Fatal("Portal proxy did not reach the cancellable Token handler")
			}
			if !deadline.Equal(businessDeadline) {
				t.Fatalf("Portal/Token reset the absolute deadline: got=%s want=%s", deadline, businessDeadline)
			}
			if response.Code != item.status || !strings.Contains(response.Body.String(), item.code) {
				t.Fatalf("stable error was lost: status=%d body=%s", response.Code, response.Body.String())
			}
			if elapsed := time.Since(started); elapsed < 100*time.Millisecond || elapsed >= 2*time.Second {
				t.Fatalf("Token deadline was not consumed before the outer BFF/Portal deadline: %s", elapsed)
			}
		})
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

func TestSessionEndpointExposesOnlyNonSensitiveAdminModelTokenIAGate(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	service.config.AdminModelTokenIAEnabled = true
	session := Session{ID: "feature-session", AccessToken: "access-secret", RefreshToken: "refresh-secret", AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(2 * time.Hour), CSRFToken: strings.Repeat("f", 32)}
	_ = store.SaveSession(context.Background(), session)
	request := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)
	if body := response.Body.String(); response.Code != http.StatusOK || !strings.Contains(body, `"admin_model_token_ia":true`) || strings.Contains(body, "access-secret") || strings.Contains(body, "refresh-secret") {
		t.Fatalf("unexpected feature session response: %d %s", response.Code, body)
	}
}

func TestSessionEndpointExposesOnlyNonSensitiveTokenResourceFormsGate(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	service.config.AdminTokenResourceFormsEnabled = true
	session := Session{ID: "token-resource-feature-session", AccessToken: "access-secret", RefreshToken: "refresh-secret", AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(2 * time.Hour), CSRFToken: strings.Repeat("t", 32)}
	_ = store.SaveSession(context.Background(), session)
	request := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)
	if body := response.Body.String(); response.Code != http.StatusOK || !strings.Contains(body, `"admin_token_resource_forms":true`) || strings.Contains(body, "access-secret") || strings.Contains(body, "refresh-secret") {
		t.Fatalf("unexpected token resource feature session response: %d %s", response.Code, body)
	}
}

func TestSessionEndpointExposesOnlyNonSensitiveModelOperationsDetailsGate(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	service.config.AdminModelOperationsDetailsEnabled = true
	session := Session{ID: "model-operations-feature-session", AccessToken: "access-secret", RefreshToken: "refresh-secret", AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(2 * time.Hour), CSRFToken: strings.Repeat("o", 32)}
	_ = store.SaveSession(context.Background(), session)
	request := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)
	if body := response.Body.String(); response.Code != http.StatusOK || !strings.Contains(body, `"admin_model_operations_details":true`) || strings.Contains(body, "access-secret") || strings.Contains(body, "refresh-secret") {
		t.Fatalf("unexpected model operations feature session response: %d %s", response.Code, body)
	}
}

func TestSessionEndpointExposesOnlyNonSensitiveModelOperationsOverviewGate(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	service.config.AdminModelOperationsOverviewEnabled = true
	session := Session{ID: "model-operations-overview-session", AccessToken: "access-secret", RefreshToken: "refresh-secret", AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(2 * time.Hour), CSRFToken: strings.Repeat("v", 32)}
	_ = store.SaveSession(context.Background(), session)
	request := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)
	if body := response.Body.String(); response.Code != http.StatusOK || !strings.Contains(body, `"admin_model_operations_overview":true`) || strings.Contains(body, "access-secret") || strings.Contains(body, "refresh-secret") {
		t.Fatalf("unexpected model operations overview feature session response: %d %s", response.Code, body)
	}
}

func TestSessionEndpointExposesOnlyNonSensitiveModelTokenPageOverviewsGate(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	service.config.AdminModelTokenPageOverviewsEnabled = true
	service.config.TokenAdminProxyHMACKey = []byte(strings.Repeat("p", 32))
	session := Session{ID: "model-token-page-overviews-session", AccessToken: "access-secret", RefreshToken: "refresh-secret", AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(2 * time.Hour), CSRFToken: strings.Repeat("w", 32)}
	_ = store.SaveSession(context.Background(), session)
	request := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)
	if body := response.Body.String(); response.Code != http.StatusOK || !strings.Contains(body, `"admin_model_token_page_overviews":true`) || strings.Contains(body, "access-secret") || strings.Contains(body, "refresh-secret") {
		t.Fatalf("unexpected model and Token page overview feature session response: %d %s", response.Code, body)
	}
}

func TestModelTokenPageOverviewsGateRequiresStrongProxyKey(t *testing.T) {
	store := newMemoryStore()
	config := testService(t, store).config
	config.AdminModelTokenPageOverviewsEnabled = true
	config.TokenAdminProxyHMACKey = []byte("short")
	if _, err := NewService(config, store); err == nil || !strings.Contains(err.Error(), "at least 32 bytes") {
		t.Fatalf("expected page overview proxy key rejection, got %v", err)
	}
}

func TestSessionEndpointExposesOnlyNonSensitiveModelDetailsGate(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	service.config.AdminModelDetailsEnabled = true
	session := Session{ID: "model-details-feature-session", AccessToken: "access-secret", RefreshToken: "refresh-secret", AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(2 * time.Hour), CSRFToken: strings.Repeat("d", 32)}
	_ = store.SaveSession(context.Background(), session)
	request := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)
	if body := response.Body.String(); response.Code != http.StatusOK || !strings.Contains(body, `"admin_model_details":true`) || strings.Contains(body, "access-secret") || strings.Contains(body, "refresh-secret") {
		t.Fatalf("unexpected model details feature session response: %d %s", response.Code, body)
	}
}

func TestSessionEndpointExposesOnlyNonSensitiveModelPricingEditGate(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	service.config.AdminModelPricingEditEnabled = true
	session := Session{ID: "model-pricing-edit-session", AccessToken: "access-secret", RefreshToken: "refresh-secret", AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(2 * time.Hour), CSRFToken: strings.Repeat("p", 32)}
	_ = store.SaveSession(context.Background(), session)
	request := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)
	if body := response.Body.String(); response.Code != http.StatusOK || !strings.Contains(body, `"admin_model_pricing_edit":true`) || strings.Contains(body, "access-secret") || strings.Contains(body, "refresh-secret") {
		t.Fatalf("unexpected model pricing edit feature session response: %d %s", response.Code, body)
	}
}

func TestSessionEndpointExposesOnlyNonSensitiveModelTokenActionsGate(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	service.config.AdminModelTokenActionsEnabled = true
	session := Session{ID: "model-token-actions-session", AccessToken: "access-secret", RefreshToken: "refresh-secret", AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(2 * time.Hour), CSRFToken: strings.Repeat("a", 32)}
	_ = store.SaveSession(context.Background(), session)
	request := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)
	if body := response.Body.String(); response.Code != http.StatusOK || !strings.Contains(body, `"admin_model_token_actions":true`) || strings.Contains(body, "access-secret") || strings.Contains(body, "refresh-secret") {
		t.Fatalf("unexpected model Token actions feature session response: %d %s", response.Code, body)
	}
}

func TestSystemStatusAggregatesInternalHealthWithoutExposingEndpoints(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/account/bootstrap":
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			if r.Header.Get("X-Pinvou-Client-Version") != "0.8.0" || r.Header.Get("X-Pinvou-Client-Source") == "" {
				w.WriteHeader(http.StatusUpgradeRequired)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"platform_permissions":["platform.system.status.read"]}`)
		case "/admin/v1/overview":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"reconciliation_pending":2,"reconciliation_stopped":1}`)
		case "/admin/v1/usage-requests":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"items":[{"usage_request_id":"usage-a","reconciliation_last_error":"evidence lookup failed","updated_at":"2026-08-19T01:02:03Z"}]}`)
		case "/metrics":
			_, _ = io.WriteString(w, "pinvou_outbox_pending_events{source=\"account\"} 3\npinvou_outbox_dead_lettered_events{source=\"account\"} 0\npinvou_outbox_oldest_pending_age_seconds{source=\"account\"} 12.5\n")
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer upstream.Close()
	parsed, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	store := newMemoryStore()
	service, err := NewService(Config{
		PublicURL:        parsed,
		AuthorizationURL: parsed,
		TokenURL:         parsed,
		ClientID:         "portal",
		ClientVersion:    "0.8.0",
		AccountURL:       parsed,
		BillingURL:       parsed,
		TokenGatewayURL:  parsed,
		SystemProbes: []SystemProbe{
			{Key: "portal", Kind: "local"},
			{Key: "identity", Kind: "http", Target: endpoint(parsed, "/keycloak")},
			{Key: "model-gateway", Kind: "http", Target: endpoint(parsed, "/newapi")},
			{Key: "event-bus", Kind: "http", Target: endpoint(parsed, "/nats")},
			{Key: "account-event-relay", Kind: "relay", Target: parsed},
		},
	}, store)
	if err != nil {
		t.Fatal(err)
	}
	session := Session{ID: "system-session", AccessToken: "admin-access", RefreshToken: "refresh", AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(2 * time.Hour), CSRFToken: strings.Repeat("s", 32)}
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/admin/system/status", nil)
	request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("system status = %d %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, `"pending":2`) || !strings.Contains(body, `"pending_events":3`) || !strings.Contains(body, "evidence lookup failed") {
		t.Fatalf("missing system status data: %s", body)
	}
	if strings.Contains(body, upstream.URL) || strings.Contains(body, "admin-access") {
		t.Fatalf("system status exposed an internal endpoint or access token: %s", body)
	}
}

func TestParseRelayMetricsRequiresAllBacklogSignals(t *testing.T) {
	metrics, err := parseRelayMetrics(strings.NewReader("pinvou_outbox_pending_events 4\npinvou_outbox_dead_lettered_events 1\npinvou_outbox_oldest_pending_age_seconds 8.5\n"))
	if err != nil || metrics["pinvou_outbox_pending_events"] != 4 || metrics["pinvou_outbox_oldest_pending_age_seconds"] != 8.5 {
		t.Fatalf("unexpected relay metrics: %#v, %v", metrics, err)
	}
	if _, err := parseRelayMetrics(strings.NewReader("pinvou_outbox_pending_events 1\n")); err == nil {
		t.Fatal("incomplete relay metrics must be rejected")
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
	organizationID := uuid.Must(uuid.NewV7()).String()
	grantID := uuid.Must(uuid.NewV7()).String()
	orderID := uuid.Must(uuid.NewV7()).String()
	if !tokenPathAllowed(http.MethodPost, "/v1/organizations/"+organizationID+"/recharge/orders") || !tokenPathAllowed(http.MethodGet, "/v1/organizations/"+organizationID+"/recharge/orders/"+orderID) || !tokenPathAllowed(http.MethodGet, "/v1/organizations/"+organizationID+"/balance") || !tokenPathAllowed(http.MethodGet, "/v1/organizations/"+organizationID+"/entitlement-packages") || !tokenPathAllowed(http.MethodGet, "/v1/organizations/"+organizationID+"/quota-grants") || !tokenPathAllowed(http.MethodPost, "/v1/organizations/"+organizationID+"/quota-grants:batch") || !tokenPathAllowed(http.MethodPost, "/v1/organizations/"+organizationID+"/quota-grants/"+grantID+"/revoke") || !tokenPathAllowed(http.MethodPost, "/v1/organizations/"+organizationID+"/account-closed-grants/"+grantID+"/reclaim") || !tokenPathAllowed(http.MethodPost, "/v1/organizations/"+organizationID+"/account-closed-grants:reclaim-batch") {
		t.Fatal("organization billing paths must be allowlisted")
	}
	if !tokenPathAllowed(http.MethodGet, "/v1/me/balance") || !tokenPathAllowed(http.MethodGet, "/v1/me/usage-requests") || !accountPathAllowed(http.MethodGet, "/v1/me") {
		t.Fatal("Portal BFF must expose the documented management APIs")
	}
	if !tokenPathAllowed(http.MethodPost, "/v1/recharge/orders/"+orderID+"/payment-claims") || tokenPathAllowed(http.MethodPost, "/v1/recharge/orders/invalid/payment-claims") {
		t.Fatal("Portal BFF must expose payment claims only for a valid order path")
	}
	for _, resource := range []string{"/v1/token-accounts", "/v1/token-accounts/id/credentials", "/v1/token-accounts/eligible", "/v1/usage-requests/id"} {
		if tokenPathAllowed(http.MethodGet, resource) {
			t.Fatalf("Portal BFF must not expose internal Token management route %s", resource)
		}
	}
	if safeReturnTo("//evil.example") != "/" || safeReturnTo("https://evil.example") != "/" {
		t.Fatal("external post-login redirects must be rejected")
	}
}

func TestAdminProxyAllowsOnlyDocumentedManagementEndpoints(t *testing.T) {
	if !accountAdminPathAllowed(http.MethodGet, "/admin/v1/users") || !accountAdminPathAllowed(http.MethodGet, "/admin/v1/roles") || !accountAdminPathAllowed(http.MethodGet, "/admin/v1/role-bindings") || !billingAdminPathAllowed(http.MethodGet, "/admin/v1/wallets") || !tokenAdminPathAllowed(http.MethodGet, "/admin/v1/token-accounts") {
		t.Fatal("admin portal must expose the documented management APIs")
	}
	userID := "0198fbf7-c3ba-7bc4-8849-c157740963de"
	organizationID := "0198fbf7-c3ba-7bc4-8849-c157740963df"
	unitID := "0198fbf7-c3ba-7bc4-8849-c157740963e0"
	memberID := "0198fbf7-c3ba-7bc4-8849-c157740963e1"
	bindingID := "0198fbf7-c3ba-7bc4-8849-c157740963e2"
	if !accountAdminPathAllowed(http.MethodPost, "/admin/v1/role-bindings") || !accountAdminPathAllowed(http.MethodDelete, "/admin/v1/role-bindings/"+bindingID) {
		t.Fatal("admin portal must expose role binding grant and single-binding revoke")
	}
	if !accountAdminPathAllowed(http.MethodGet, "/admin/v1/user-code-lookups/USR-0123-4567-89AB-CDEF-MV") {
		t.Fatal("admin portal must expose complete user-code lookup")
	}
	if !accountAdminPathAllowed(http.MethodPatch, "/admin/v1/users/"+userID+"/status") || !accountAdminPathAllowed(http.MethodPost, "/admin/v1/organizations") || !accountAdminPathAllowed(http.MethodPatch, "/admin/v1/organizations/"+organizationID) || !accountAdminPathAllowed(http.MethodPatch, "/admin/v1/organizations/"+organizationID+"/status") || !accountAdminPathAllowed(http.MethodDelete, "/admin/v1/organizations/"+organizationID) || !accountAdminPathAllowed(http.MethodGet, "/admin/v1/organizations/"+organizationID+"/deletion-readiness") || !accountAdminPathAllowed(http.MethodGet, "/admin/v1/organizations/"+organizationID+"/members") || !accountAdminPathAllowed(http.MethodPost, "/admin/v1/organizations/"+organizationID+"/ownership") || !accountAdminPathAllowed(http.MethodGet, "/admin/v1/organizations/"+organizationID+"/units") || !accountAdminPathAllowed(http.MethodPost, "/admin/v1/organizations/"+organizationID+"/units") || !accountAdminPathAllowed(http.MethodPatch, "/admin/v1/organizations/"+organizationID+"/units/"+unitID) || !accountAdminPathAllowed(http.MethodDelete, "/admin/v1/organizations/"+organizationID+"/units/"+unitID) {
		t.Fatal("admin portal must expose account lifecycle mutations")
	}
	if !accountAdminPathAllowed(http.MethodGet, "/admin/v1/account-closure-requests") || !accountAdminPathAllowed(http.MethodPost, "/admin/v1/account-closure-requests/"+userID+"/actions/retry") {
		t.Fatal("account closure operations routes must be allowlisted")
	}
	if !accountAdminPathAllowed(http.MethodGet, "/admin/v1/users/"+userID+"/memberships") || !accountAdminPathAllowed(http.MethodPost, "/admin/v1/organizations/"+organizationID+"/members") || !accountAdminPathAllowed(http.MethodPut, "/admin/v1/organizations/"+organizationID+"/members/"+memberID+"/roles") || !accountAdminPathAllowed(http.MethodDelete, "/admin/v1/organizations/"+organizationID+"/members/"+memberID) {
		t.Fatal("admin portal must expose documented membership and role operations")
	}
	if !accountAdminPathAllowed(http.MethodGet, "/admin/v1/audit-logs/"+userID) {
		t.Fatal("admin portal must expose audit event details")
	}
	if !accountAdminPathAllowed(http.MethodGet, "/admin/v1/registration-operations") || !accountAdminPathAllowed(http.MethodPost, "/admin/v1/registration-operations/"+userID+"/actions/retry") || !accountAdminPathAllowed(http.MethodPost, "/admin/v1/registration-operations/"+userID+"/actions/compensate") {
		t.Fatal("admin portal must expose proof-gated registration repair operations")
	}
	if !accountAdminPathAllowed(http.MethodGet, "/admin/v1/profile-change-requests") || !accountAdminPathAllowed(http.MethodPost, "/admin/v1/profile-change-requests/"+userID+"/actions/retry") || accountAdminPathAllowed(http.MethodPost, "/admin/v1/profile-change-requests/not-a-uuid/actions/retry") {
		t.Fatal("admin portal must expose only valid profile-change repair operations")
	}
	if billingAdminPathAllowed(http.MethodPost, "/admin/v1/pricing-versions") || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/adjustments") || !billingAdminPathAllowed(http.MethodGet, "/admin/v1/refunds") || !billingAdminPathAllowed(http.MethodGet, "/admin/v1/wallets/"+userID+"/ledger") || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/payment-orders/"+userID+"/sync") || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/adjustments/"+userID+"/approve") || !billingAdminPathAllowed(http.MethodGet, "/admin/v1/usage-holds/"+userID) || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/usage-holds/"+userID+"/release") || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/reconciliation-runs") || !billingAdminPathAllowed(http.MethodGet, "/admin/v1/reconciliation-runs/"+userID+"/cases") || !billingAdminPathAllowed(http.MethodGet, "/admin/v1/late-payment-reviews") || !billingAdminPathAllowed(http.MethodGet, "/admin/v1/late-payment-reviews/"+userID) || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/late-payment-reviews/"+userID+"/query") || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/reconciliation-imports") {
		t.Fatal("admin portal must expose documented billing operations")
	}
	if !billingAdminPathAllowed(http.MethodGet, "/admin/v1/metering-units") || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/pricing/drafts") || !billingAdminPathAllowed(http.MethodPut, "/admin/v1/pricing/"+userID+"/draft") || !billingAdminPathAllowed(http.MethodGet, "/admin/v1/pricing/"+userID+"/preview") || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/pricing/"+userID+"/publish") || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/pricing/"+userID+"/transition") || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/metering-units/"+userID+"/deactivate") {
		t.Fatal("admin portal must expose versioned metering and pricing lifecycle operations")
	}
	if !billingAdminPathAllowed(http.MethodGet, "/admin/v1/recharge-products") || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/recharge-products") || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/recharge-products/"+userID+"/publish") || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/recharge-products/"+userID+"/unpublish") || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/recharge-products/"+userID+"/delete") || !billingAdminPathAllowed(http.MethodGet, "/admin/v1/recharge-products/"+userID+"/admission-verifications") || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/recharge-products/"+userID+"/admission-verifications/"+memberID+"/retry") || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/recharge-products/"+userID+"/admission-verifications/"+memberID+"/revoke") {
		t.Fatal("admin portal must expose only the recharge product catalog lifecycle")
	}
	for _, resource := range []string{"accounts", "payments", "pricing", "products"} {
		path := "/admin/v1/finance-overviews/" + resource
		if !billingAdminPathAllowed(http.MethodGet, path) || billingAdminPathAllowed(http.MethodPost, path) {
			t.Fatalf("admin portal must expose %s as read-only", path)
		}
	}
	if !accountAdminPathAllowed(http.MethodGet, "/admin/v1/approval-policies") || !accountAdminPathAllowed(http.MethodGet, "/admin/v1/approval-requests") || !accountAdminPathAllowed(http.MethodPost, "/admin/v1/approval-requests") || !accountAdminPathAllowed(http.MethodPut, "/admin/v1/approval-requests/"+userID) || !accountAdminPathAllowed(http.MethodPost, "/admin/v1/approval-requests/"+userID+"/actions/decide") {
		t.Fatal("approval workflow paths must be allowlisted")
	}
	if tokenAdminPathAllowed(http.MethodPost, "/admin/v1/token-accounts") || !tokenAdminPathAllowed(http.MethodGet, "/admin/v1/token-accounts/"+userID) || !tokenAdminPathAllowed(http.MethodPut, "/admin/v1/token-accounts/"+userID+"/routes") || !tokenAdminPathAllowed(http.MethodDelete, "/admin/v1/token-accounts/"+userID+"/routes") || !tokenAdminPathAllowed(http.MethodGet, "/admin/v1/usage-requests/"+userID) {
		t.Fatal("admin portal must expose Token reads and the APT-125 PRE route contract without exposing direct account creation")
	}
	for _, blocked := range []struct{ method, resource string }{
		{http.MethodPatch, "/admin/v1/token-accounts/" + userID + "/status"},
		{http.MethodPost, "/admin/v1/token-accounts/" + userID + "/credentials/" + memberID + "/validate"},
		{http.MethodPost, "/admin/v1/usage-requests/" + userID + "/retry-reconciliation"},
		{http.MethodPost, "/admin/v1/provisionings/" + userID + "/repair"},
		{http.MethodPost, "/admin/v1/models:sync"},
		{http.MethodPost, "/admin/v1/model-catalog:rollback"},
		{http.MethodPost, "/admin/v1/models/" + userID + "/publish"},
		{http.MethodPost, "/admin/v1/models/" + userID + "/lifecycle"},
	} {
		if tokenAdminPathAllowed(blocked.method, blocked.resource) {
			t.Fatalf("ungoverned model/Token mutation remained exposed: %s %s", blocked.method, blocked.resource)
		}
	}
	if !tokenAdminPathAllowed(http.MethodGet, "/admin/v1/protocols") || !tokenAdminPathAllowed(http.MethodGet, "/admin/v1/formulas/"+userID+"/preview") || !tokenAdminPathAllowed(http.MethodPost, "/admin/v1/commands:precheck") || !tokenAdminPathAllowed(http.MethodPost, "/admin/v1/commands") || !tokenAdminPathAllowed(http.MethodGet, "/admin/v1/commands/"+userID) {
		t.Fatal("admin portal must expose governed protocol/formula reads, prechecks, commands, and recovery")
	}
	if !tokenAdminPathAllowed(http.MethodGet, "/admin/v1/commands/by-idempotency-key") || !tokenAdminPathAllowed(http.MethodPost, "/admin/v1/commands/"+userID+"/recovery-actions") || !tokenAdminPathAllowed(http.MethodGet, "/admin/v1/commands/"+userID+"/recovery-actions/by-idempotency-key") || !tokenAdminPathAllowed(http.MethodGet, "/admin/v1/commands/"+userID+"/recovery-actions/"+memberID) {
		t.Fatal("Token Account provisioning recovery paths must remain narrowly allowlisted")
	}
	if tokenAdminPathAllowed(http.MethodPost, "/admin/v1/protocols") || tokenAdminPathAllowed(http.MethodPost, "/admin/v1/protocols/"+userID+"/publish") || tokenAdminPathAllowed(http.MethodPost, "/admin/v1/protocols/"+userID+"/formulas") || tokenAdminPathAllowed(http.MethodPost, "/admin/v1/formulas/"+userID+"/publish") {
		t.Fatal("legacy direct protocol/formula writes must not remain exposed")
	}
	if accountAdminPathAllowed(http.MethodPost, "/admin/v1/users") || accountAdminPathAllowed(http.MethodDelete, "/admin/v1/users/"+userID) || accountAdminPathAllowed(http.MethodDelete, "/admin/v1/users/not-a-uuid") || accountAdminPathAllowed(http.MethodPost, "/admin/v1/registration-operations/"+userID+"/actions/force-complete") || accountAdminPathAllowed(http.MethodPost, "/admin/v1/organizations/"+organizationID+"/members/"+memberID) || accountAdminPathAllowed(http.MethodGet, "/admin/v1/organizations/"+organizationID+"/ownership") || accountAdminPathAllowed(http.MethodDelete, "/admin/v1/organizations/"+organizationID+"/units/not-a-uuid") || billingAdminPathAllowed(http.MethodGet, "/internal/wallets") || tokenAdminPathAllowed(http.MethodGet, "/v1/chat/completions") {
		t.Fatal("admin portal must not expose undocumented mutations, internal APIs, or the model data plane")
	}
}

func TestModelTokenActionGateRejectsLegacyV2AndAccountApprovalWritesBeforeProxy(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	service.config.AdminModelTokenActionsEnabled = false
	upstreamHits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	service.config.TokenGatewayURL = upstreamURL
	service.config.AccountURL = upstreamURL
	csrf := strings.Repeat("c", 32)
	session := Session{ID: "model-token-gate", AccessToken: "access", RefreshToken: "refresh", AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(2 * time.Hour), CSRFToken: csrf}
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	authorize := func(request *http.Request) {
		request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
		request.AddCookie(&http.Cookie{Name: service.csrfCookieName(), Value: csrf})
		request.Header.Set("Origin", "https://portal.example")
		request.Header.Set("X-CSRF-Token", csrf)
	}
	for _, item := range []struct {
		name     string
		method   string
		resource string
		body     string
		account  bool
		status   int
	}{
		{name: "legacy token write", method: http.MethodPost, resource: "admin/v1/models:sync", status: http.StatusNotFound},
		{name: "v2 token write", method: http.MethodPut, resource: "admin/v2/token-accounts/0198fbf7-c3ba-7bc4-8849-c157740963de/routes", status: http.StatusNotFound},
		{name: "lifecycle plan create", method: http.MethodPost, resource: "admin/v1/models/0198fbf7-c3ba-7bc4-8849-c157740963de/lifecycle-plans", status: http.StatusNotFound},
		{name: "lifecycle plan cancel", method: http.MethodPost, resource: "admin/v1/models/0198fbf7-c3ba-7bc4-8849-c157740963de/lifecycle-plans/0198fbf7-c3ba-7bc4-8849-c157740963df/cancel", status: http.StatusNotFound},
		{name: "token approval draft", method: http.MethodPost, resource: "admin/v1/approval-requests", body: `{"command_type":"TOKEN_MODEL_LIFECYCLE_CHANGE"}`, account: true, status: http.StatusNotFound},
		{name: "unclassified approval draft", method: http.MethodPost, resource: "admin/v1/approval-requests", body: `{"reason":"missing command classification"}`, account: true, status: http.StatusServiceUnavailable},
		{name: "malformed approval draft", method: http.MethodPost, resource: "admin/v1/approval-requests", body: `{"command_type":`, account: true, status: http.StatusServiceUnavailable},
		{name: "unclassified approval update", method: http.MethodPut, resource: "admin/v1/approval-requests/0198fbf7-c3ba-7bc4-8849-c157740963de", body: `{"expected_version":1}`, account: true, status: http.StatusServiceUnavailable},
	} {
		t.Run(item.name, func(t *testing.T) {
			request := httptest.NewRequest(item.method, "/", strings.NewReader(item.body))
			request.SetPathValue("resource", item.resource)
			authorize(request)
			response := httptest.NewRecorder()
			if item.account {
				service.proxyAdminAccount(response, request)
			} else {
				service.proxyAdminToken(response, request)
			}
			if response.Code != item.status {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
	if upstreamHits != 0 {
		t.Fatalf("disabled model/Token actions reached upstream %d times", upstreamHits)
	}

	for _, item := range []struct {
		name     string
		method   string
		resource string
		body     string
		account  bool
	}{
		{name: "token read remains routed", method: http.MethodGet, resource: "admin/v1/models"},
		{name: "non-token approval remains routed", method: http.MethodPost, resource: "admin/v1/approval-requests", body: `{"command_type":"WALLET_ADJUSTMENT"}`, account: true},
	} {
		t.Run(item.name, func(t *testing.T) {
			request := httptest.NewRequest(item.method, "/", strings.NewReader(item.body))
			request.SetPathValue("resource", item.resource)
			authorize(request)
			response := httptest.NewRecorder()
			if item.account {
				service.proxyAdminAccount(response, request)
			} else {
				service.proxyAdminToken(response, request)
			}
			if response.Code != http.StatusNoContent {
				t.Fatalf("request was unexpectedly intercepted: status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
	if upstreamHits != 2 {
		t.Fatalf("allowed reads and non-Token approvals reached upstream %d times, want 2", upstreamHits)
	}
}

func TestTokenCredentialDetailGateRejectsNewIsolationWorkButPreservesLaunchCleanup(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	service.config.AdminTokenDetailsEnabled = false
	upstreamHits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	service.config.TokenGatewayURL = upstreamURL
	accountID := "0198fbf7-c3ba-7bc4-8849-c157740963de"
	resultRef := "0198fbf7-c3ba-7bc4-8849-c157740963df"
	for _, item := range []struct{ method, resource string }{
		{http.MethodGet, "admin/v1/token-accounts/" + accountID + "/credentials"},
		{http.MethodPost, "admin/v1/token-accounts/" + accountID + "/credential-launches/enroll"},
		{http.MethodPost, "admin/v1/token-accounts/" + accountID + "/credential-launches/rotate"},
		{http.MethodGet, "admin/v1/credential-results/" + resultRef},
	} {
		request := httptest.NewRequest(item.method, "/", nil)
		request.SetPathValue("resource", item.resource)
		response := httptest.NewRecorder()
		service.proxyAdminToken(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s %s status=%d body=%s", item.method, item.resource, response.Code, response.Body.String())
		}
	}
	if upstreamHits != 0 {
		t.Fatalf("disabled Token credential detail reached upstream %d times", upstreamHits)
	}
	csrf := strings.Repeat("c", 32)
	session := Session{ID: "credential-cleanup", AccessToken: "access", RefreshToken: "refresh", AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(2 * time.Hour), CSRFToken: csrf}
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	cleanup := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"reason_code":"CREDENTIAL_FEATURE_DISABLED"}`))
	cleanup.Header.Set("Content-Type", "application/json")
	cleanup.Header.Set("Origin", "https://portal.example")
	cleanup.Header.Set("X-CSRF-Token", csrf)
	cleanup.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
	cleanup.AddCookie(&http.Cookie{Name: service.csrfCookieName(), Value: csrf})
	cleanup.SetPathValue("resource", "admin/v1/credential-results/"+resultRef+"/revoke-window")
	cleanupResponse := httptest.NewRecorder()
	service.proxyAdminToken(cleanupResponse, cleanup)
	if cleanupResponse.Code != http.StatusNoContent || upstreamHits != 1 {
		t.Fatalf("disabled detail gate blocked launch cleanup: status=%d hits=%d body=%s", cleanupResponse.Code, upstreamHits, cleanupResponse.Body.String())
	}
	if tokenCredentialIsolationPath(http.MethodGet, "/admin/v1/token-accounts/not-a-uuid/credentials") || tokenCredentialIsolationPath(http.MethodPost, "/admin/v1/token-accounts/"+accountID+"/routes") {
		t.Fatal("credential detail gate classified an invalid or unrelated path")
	}
}

func TestDisabledModelTokenApprovalActionsUseSessionBoundLocalClassification(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	service.config.AdminModelTokenActionsEnabled = true
	now := time.Now().UTC()
	service.now = func() time.Time { return now }
	tokenID := uuid.MustParse("0198fbf7-c3ba-7bc4-8849-c157740963de")
	financeID := uuid.MustParse("0198fbf7-c3ba-7bc4-8849-c157740963df")
	updatedFinanceID := uuid.MustParse("0198fbf7-c3ba-7bc4-8849-c157740963e1")
	accountCalls := 0
	actionWrites := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accountCalls++
		if strings.Contains(r.URL.Path, "/actions/") {
			actionWrites++
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodGet || r.Method == http.MethodPut || r.Method == http.MethodPost {
			id := path.Base(r.URL.Path)
			if r.Method == http.MethodPost {
				id = financeID.String()
				w.WriteHeader(http.StatusCreated)
			}
			commandType := "TOKEN_MODEL_LIFECYCLE_CHANGE"
			if id == financeID.String() || id == updatedFinanceID.String() {
				commandType = "WALLET_ADJUSTMENT"
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"approval_request_id":"`+id+`","command_type":"`+commandType+`"}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	service.config.AccountURL, _ = url.Parse(upstream.URL)
	csrf := strings.Repeat("z", 32)
	session := Session{ID: "actor-a", AccessToken: "access", RefreshToken: "refresh", AccessExpiresAt: now.Add(time.Hour), RefreshExpiresAt: now.Add(2 * time.Hour), CSRFToken: csrf}
	_ = store.SaveSession(context.Background(), session)
	authorize := func(request *http.Request, value Session) {
		request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: value.ID})
		request.AddCookie(&http.Cookie{Name: service.csrfCookieName(), Value: value.CSRFToken})
		request.Header.Set("Origin", "https://portal.example")
		request.Header.Set("X-CSRF-Token", value.CSRFToken)
	}
	read := func(id uuid.UUID) {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.SetPathValue("resource", "admin/v1/approval-requests/"+id.String())
		authorize(request, session)
		response := httptest.NewRecorder()
		service.proxyAdminAccount(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("classification read status=%d body=%s", response.Code, response.Body.String())
		}
	}
	writeDraft := func(method string, id *uuid.UUID) *httptest.ResponseRecorder {
		resource := "admin/v1/approval-requests"
		if id != nil {
			resource += "/" + id.String()
		}
		request := httptest.NewRequest(method, "/", strings.NewReader(`{"command_type":"WALLET_ADJUSTMENT","expected_version":1}`))
		request.SetPathValue("resource", resource)
		authorize(request, session)
		response := httptest.NewRecorder()
		service.proxyAdminAccount(response, request)
		return response
	}
	action := func(id uuid.UUID, value Session, action string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"expected_version":1}`))
		request.SetPathValue("resource", "admin/v1/approval-requests/"+id.String()+"/actions/"+action)
		authorize(request, value)
		response := httptest.NewRecorder()
		service.proxyAdminAccount(response, request)
		return response
	}

	read(tokenID)
	if response := writeDraft(http.MethodPost, nil); response.Code != http.StatusCreated {
		t.Fatalf("classification create status=%d body=%s", response.Code, response.Body.String())
	}
	if response := writeDraft(http.MethodPut, &updatedFinanceID); response.Code != http.StatusOK {
		t.Fatalf("classification update status=%d body=%s", response.Code, response.Body.String())
	}
	service.config.AdminModelTokenActionsEnabled = false
	if response := writeDraft(http.MethodPut, &financeID); response.Code != http.StatusOK {
		t.Fatalf("non-Token update after create status=%d body=%s", response.Code, response.Body.String())
	}
	before := accountCalls
	if response := action(tokenID, session, "submit"); response.Code != http.StatusNotFound {
		t.Fatalf("Token action status=%d body=%s", response.Code, response.Body.String())
	}
	if accountCalls != before || actionWrites != 0 {
		t.Fatalf("Token action contacted Account: calls=%d->%d writes=%d", before, accountCalls, actionWrites)
	}
	if response := action(financeID, session, "submit"); response.Code != http.StatusNoContent || actionWrites != 1 {
		t.Fatalf("non-Token action status=%d writes=%d body=%s", response.Code, actionWrites, response.Body.String())
	}
	if response := action(financeID, session, "withdraw"); response.Code != http.StatusNoContent || actionWrites != 2 {
		t.Fatalf("non-Token withdraw status=%d writes=%d body=%s", response.Code, actionWrites, response.Body.String())
	}
	if response := action(financeID, session, "decide"); response.Code != http.StatusNoContent || actionWrites != 3 {
		t.Fatalf("non-Token decide status=%d writes=%d body=%s", response.Code, actionWrites, response.Body.String())
	}
	if response := action(updatedFinanceID, session, "submit"); response.Code != http.StatusNoContent || actionWrites != 4 {
		t.Fatalf("updated non-Token action status=%d writes=%d body=%s", response.Code, actionWrites, response.Body.String())
	}
	rotated := session
	rotated.AccessToken = "different-actor-token"
	_ = store.SaveSession(context.Background(), rotated)
	before = accountCalls
	if response := action(financeID, rotated, "submit"); response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "APPROVAL_CLASSIFICATION_UNAVAILABLE") {
		t.Fatalf("cross-actor token proof status=%d body=%s", response.Code, response.Body.String())
	}
	if accountCalls != before || actionWrites != 4 {
		t.Fatal("cross-actor token proof must fail before Account")
	}
	_ = store.SaveSession(context.Background(), session)

	other := Session{ID: "actor-b", AccessToken: "access", RefreshToken: "refresh", AccessExpiresAt: now.Add(time.Hour), RefreshExpiresAt: now.Add(2 * time.Hour), CSRFToken: strings.Repeat("y", 32)}
	_ = store.SaveSession(context.Background(), other)
	before = accountCalls
	if response := action(financeID, other, "submit"); response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "APPROVAL_CLASSIFICATION_UNAVAILABLE") {
		t.Fatalf("cross-actor proof status=%d body=%s", response.Code, response.Body.String())
	}
	if accountCalls != before || actionWrites != 4 {
		t.Fatal("cross-actor proof must fail before Account")
	}
	proof, _ := store.GetApprovalClassification(context.Background(), session.ID, financeID.String())
	proof.ExpiresAt = now.Add(-time.Second)
	_ = store.SaveApprovalClassification(context.Background(), proof)
	if response := action(financeID, session, "submit"); response.Code != http.StatusServiceUnavailable || actionWrites != 4 {
		t.Fatalf("expired proof status=%d writes=%d", response.Code, actionWrites)
	}
	forgedID := uuid.MustParse("0198fbf7-c3ba-7bc4-8849-c157740963e0")
	if response := action(forgedID, session, "submit"); response.Code != http.StatusServiceUnavailable || actionWrites != 4 {
		t.Fatalf("missing/forged proof status=%d writes=%d", response.Code, actionWrites)
	}
}

func TestPortalClientSourceUsesValidatedProxyHeaders(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "10.0.0.8:4321"
	if source := portalClientSource(request); source != "10.0.0.8" {
		t.Fatalf("source=%q", source)
	}
	request.Header.Set("X-Forwarded-For", "192.0.2.20, 10.0.0.8")
	if source := portalClientSource(request); source != "192.0.2.20" {
		t.Fatalf("forwarded source=%q", source)
	}
	request.Header.Set("X-Real-IP", "198.51.100.9")
	if source := portalClientSource(request); source != "198.51.100.9" {
		t.Fatalf("real source=%q", source)
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
