package portal

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
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
	locks    map[string]string
}

func newMemoryStore() *memoryStore {
	return &memoryStore{flows: map[string]LoginFlow{}, sessions: map[string]Session{}, locks: map[string]string{}}
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
	return nil
}
func (s *memoryStore) TryRefreshLock(_ context.Context, id, owner string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.locks[id]; exists {
		return false, nil
	}
	s.locks[id] = owner
	return true, nil
}
func (s *memoryStore) ReleaseRefreshLock(_ context.Context, id, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locks[id] == owner {
		delete(s.locks, id)
	}
	return nil
}
func (s *memoryStore) SaveRefreshedSession(_ context.Context, previous, updated Session, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.sessions[previous.ID]
	if !exists {
		return ErrSessionNotFound
	}
	if current != previous || s.locks[previous.ID] != owner {
		return ErrRefreshConflict
	}
	s.sessions[previous.ID] = updated
	return nil
}
func (s *memoryStore) DeleteRefreshedSession(_ context.Context, previous Session, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.sessions[previous.ID]
	if !exists {
		return ErrSessionNotFound
	}
	if current != previous || s.locks[previous.ID] != owner {
		return ErrRefreshConflict
	}
	delete(s.sessions, previous.ID)
	return nil
}
func (*memoryStore) Ping(context.Context) error { return nil }
func (*memoryStore) Close() error               { return nil }

func testService(t *testing.T, store Store) *Service {
	t.Helper()
	parse := func(value string) *url.URL {
		parsed, err := url.Parse(value)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	service, err := NewService(Config{Environment: "test", IssuerURL: parse("https://identity.example"), JWKSURL: parse("https://identity.internal/keys"), PublicURL: parse("https://portal.example"), AuthorizationURL: parse("https://identity.example/auth"), TokenURL: parse("https://identity.internal/token"), ClientID: "portal", AccountURL: parse("https://account.internal"), BillingURL: parse("https://billing.internal"), TokenGatewayURL: parse("https://token.internal"), SecureCookies: true}, store)
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
	if location.Query().Get("scope") != "openid profile email" {
		t.Fatal("Portal must use user-approved online OIDC scopes")
	}
	state := location.Query().Get("state")
	flow := store.flows[state]
	if flow.Verifier == "" || flow.ReturnTo != "/wallet" {
		t.Fatalf("unexpected stored flow: %+v", flow)
	}
	challenge := sha256.Sum256([]byte(flow.Verifier))
	if location.Query().Get("code_challenge") != base64.RawURLEncoding.EncodeToString(challenge[:]) || location.Query().Get("nonce") != flow.Nonce || flow.Nonce == "" {
		t.Fatal("authorization request is not bound to its server-side PKCE/nonce flow")
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

func TestSystemStatusAggregatesInternalHealthWithoutExposingEndpoints(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
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
		Environment:       "test",
		IssuerURL:         parsed,
		JWKSURL:           parsed,
		PublicURL:         parsed,
		AuthorizationURL:  parsed,
		TokenURL:          parsed,
		ClientID:          "portal",
		AccountURL:        parsed,
		BillingURL:        parsed,
		TokenGatewayURL:   parsed,
		KeycloakStatusURL: endpoint(parsed, "/keycloak"),
		NATSStatusURL:     endpoint(parsed, "/nats"),
		RelayURLs:         map[string]*url.URL{"account-event-relay": parsed, "billing-event-relay": parsed, "token-event-relay": parsed},
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

func TestPortalProxyExcludesModelDataPlane(t *testing.T) {
	if tokenPathAllowed(http.MethodPost, "/v1/chat/completions") {
		t.Fatal("Portal BFF must not expose the model data plane")
	}
	if !tokenPathAllowed(http.MethodGet, "/v1/me/balance") || !tokenPathAllowed(http.MethodGet, "/v1/me/usage-requests") || !accountPathAllowed(http.MethodGet, "/v1/me") {
		t.Fatal("Portal BFF must expose the documented management APIs")
	}
	for _, resource := range []string{"/v1/token-accounts", "/v1/token-accounts/id/credentials", "/v1/token-accounts/eligible", "/v1/usage-requests/id"} {
		if !tokenPathAllowed(http.MethodGet, resource) {
			t.Fatalf("Portal BFF must preserve user-managed Token route %s", resource)
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
	if !accountAdminPathAllowed(http.MethodPatch, "/admin/v1/users/"+userID+"/status") || !accountAdminPathAllowed(http.MethodDelete, "/admin/v1/users/"+userID) || !accountAdminPathAllowed(http.MethodPost, "/admin/v1/organizations") || !accountAdminPathAllowed(http.MethodPatch, "/admin/v1/organizations/"+organizationID) || !accountAdminPathAllowed(http.MethodPatch, "/admin/v1/organizations/"+organizationID+"/status") || !accountAdminPathAllowed(http.MethodDelete, "/admin/v1/organizations/"+organizationID) || !accountAdminPathAllowed(http.MethodGet, "/admin/v1/organizations/"+organizationID+"/members") || !accountAdminPathAllowed(http.MethodPost, "/admin/v1/organizations/"+organizationID+"/ownership") || !accountAdminPathAllowed(http.MethodGet, "/admin/v1/organizations/"+organizationID+"/units") || !accountAdminPathAllowed(http.MethodPost, "/admin/v1/organizations/"+organizationID+"/units") || !accountAdminPathAllowed(http.MethodPatch, "/admin/v1/organizations/"+organizationID+"/units/"+unitID) || !accountAdminPathAllowed(http.MethodDelete, "/admin/v1/organizations/"+organizationID+"/units/"+unitID) {
		t.Fatal("admin portal must expose account lifecycle mutations")
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
	if !billingAdminPathAllowed(http.MethodPost, "/admin/v1/pricing-versions") || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/adjustments") || !billingAdminPathAllowed(http.MethodGet, "/admin/v1/wallets/"+userID+"/ledger") || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/payment-orders/"+userID+"/sync") || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/adjustments/"+userID+"/approve") || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/usage-holds/"+userID+"/release") || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/reconciliation-runs") || !billingAdminPathAllowed(http.MethodGet, "/admin/v1/reconciliation-runs/"+userID+"/cases") {
		t.Fatal("admin portal must expose documented billing operations")
	}
	if !accountAdminPathAllowed(http.MethodGet, "/admin/v1/approval-policies") || !accountAdminPathAllowed(http.MethodGet, "/admin/v1/approval-requests") || !accountAdminPathAllowed(http.MethodPost, "/admin/v1/approval-requests") || !accountAdminPathAllowed(http.MethodPut, "/admin/v1/approval-requests/"+userID) || !accountAdminPathAllowed(http.MethodPost, "/admin/v1/approval-requests/"+userID+"/actions/decide") {
		t.Fatal("approval workflow paths must be allowlisted")
	}
	if !tokenAdminPathAllowed(http.MethodGet, "/admin/v1/token-accounts/"+userID) || !tokenAdminPathAllowed(http.MethodPatch, "/admin/v1/token-accounts/"+userID+"/status") || !tokenAdminPathAllowed(http.MethodPut, "/admin/v1/token-accounts/"+userID+"/routes") || !tokenAdminPathAllowed(http.MethodDelete, "/admin/v1/token-accounts/"+userID+"/routes") || !tokenAdminPathAllowed(http.MethodPost, "/admin/v1/token-accounts/"+userID+"/credentials/"+memberID+"/validate") || !tokenAdminPathAllowed(http.MethodGet, "/admin/v1/usage-requests/"+userID) || !tokenAdminPathAllowed(http.MethodPost, "/admin/v1/usage-requests/"+userID+"/retry-reconciliation") {
		t.Fatal("admin portal must expose documented Token Gateway operations")
	}
	if accountAdminPathAllowed(http.MethodPost, "/admin/v1/users") || accountAdminPathAllowed(http.MethodDelete, "/admin/v1/users/not-a-uuid") || accountAdminPathAllowed(http.MethodPost, "/admin/v1/registration-operations/"+userID+"/actions/force-complete") || accountAdminPathAllowed(http.MethodPost, "/admin/v1/organizations/"+organizationID+"/members/"+memberID) || accountAdminPathAllowed(http.MethodGet, "/admin/v1/organizations/"+organizationID+"/ownership") || accountAdminPathAllowed(http.MethodDelete, "/admin/v1/organizations/"+organizationID+"/units/not-a-uuid") || billingAdminPathAllowed(http.MethodGet, "/internal/wallets") || tokenAdminPathAllowed(http.MethodGet, "/v1/chat/completions") {
		t.Fatal("admin portal must not expose undocumented mutations, internal APIs, or the model data plane")
	}
}

func TestPortalClientSourceUsesPeerAndRejectsUntrustedProxyHeaders(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "10.0.0.8:4321"
	if source := portalClientSource(request); source != "10.0.0.8" {
		t.Fatalf("source=%q", source)
	}
	request.Header.Set("X-Forwarded-For", "192.0.2.20, 10.0.0.8")
	if source := portalClientSource(request); source != "10.0.0.8" {
		t.Fatalf("forwarded source=%q", source)
	}
	request.Header.Set("X-Real-IP", "198.51.100.9")
	if source := portalClientSource(request); source != "10.0.0.8" {
		t.Fatalf("real source=%q", source)
	}
	for _, peer := range []struct{ address, expected string }{
		{"[2001:db8::1]:4321", "2001:db8::1"}, {"127.0.0.1", "127.0.0.1"}, {"invalid-peer", ""}, {"", ""},
	} {
		request.RemoteAddr = peer.address
		if source := portalClientSource(request); source != peer.expected {
			t.Fatalf("peer source=%q, expected=%q", source, peer.expected)
		}
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
