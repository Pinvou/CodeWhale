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

	"github.com/google/uuid"
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
		PublicURL:        parsed,
		AuthorizationURL: parsed,
		TokenURL:         parsed,
		ClientID:         "portal",
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
	if !tokenPathAllowed(http.MethodPost, "/v1/organizations/"+organizationID+"/recharge/orders") || !tokenPathAllowed(http.MethodGet, "/v1/organizations/"+organizationID+"/balance") || !tokenPathAllowed(http.MethodGet, "/v1/organizations/"+organizationID+"/entitlement-packages") || !tokenPathAllowed(http.MethodGet, "/v1/organizations/"+organizationID+"/quota-grants") || !tokenPathAllowed(http.MethodPost, "/v1/organizations/"+organizationID+"/quota-grants:batch") || !tokenPathAllowed(http.MethodPost, "/v1/organizations/"+organizationID+"/quota-grants/"+grantID+"/revoke") || !tokenPathAllowed(http.MethodPost, "/v1/organizations/"+organizationID+"/account-closed-grants/"+grantID+"/reclaim") || !tokenPathAllowed(http.MethodPost, "/v1/organizations/"+organizationID+"/account-closed-grants:reclaim-batch") {
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
	if !accountAdminPathAllowed(http.MethodPatch, "/admin/v1/users/"+userID+"/status") || !accountAdminPathAllowed(http.MethodPost, "/admin/v1/organizations") || !accountAdminPathAllowed(http.MethodPatch, "/admin/v1/organizations/"+organizationID) || !accountAdminPathAllowed(http.MethodPatch, "/admin/v1/organizations/"+organizationID+"/status") || !accountAdminPathAllowed(http.MethodDelete, "/admin/v1/organizations/"+organizationID) || !accountAdminPathAllowed(http.MethodGet, "/admin/v1/organizations/"+organizationID+"/members") || !accountAdminPathAllowed(http.MethodPost, "/admin/v1/organizations/"+organizationID+"/ownership") || !accountAdminPathAllowed(http.MethodGet, "/admin/v1/organizations/"+organizationID+"/units") || !accountAdminPathAllowed(http.MethodPost, "/admin/v1/organizations/"+organizationID+"/units") || !accountAdminPathAllowed(http.MethodPatch, "/admin/v1/organizations/"+organizationID+"/units/"+unitID) || !accountAdminPathAllowed(http.MethodDelete, "/admin/v1/organizations/"+organizationID+"/units/"+unitID) {
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
	if !billingAdminPathAllowed(http.MethodPost, "/admin/v1/pricing-versions") || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/adjustments") || !billingAdminPathAllowed(http.MethodGet, "/admin/v1/refunds") || !billingAdminPathAllowed(http.MethodGet, "/admin/v1/wallets/"+userID+"/ledger") || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/payment-orders/"+userID+"/sync") || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/adjustments/"+userID+"/approve") || !billingAdminPathAllowed(http.MethodGet, "/admin/v1/usage-holds/"+userID) || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/usage-holds/"+userID+"/release") || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/reconciliation-runs") || !billingAdminPathAllowed(http.MethodGet, "/admin/v1/reconciliation-runs/"+userID+"/cases") || !billingAdminPathAllowed(http.MethodGet, "/admin/v1/late-payment-reviews") || !billingAdminPathAllowed(http.MethodGet, "/admin/v1/late-payment-reviews/"+userID) || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/late-payment-reviews/"+userID+"/query") || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/reconciliation-imports") {
		t.Fatal("admin portal must expose documented billing operations")
	}
	if !billingAdminPathAllowed(http.MethodGet, "/admin/v1/metering-units") || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/pricing/drafts") || !billingAdminPathAllowed(http.MethodPut, "/admin/v1/pricing/"+userID+"/draft") || !billingAdminPathAllowed(http.MethodGet, "/admin/v1/pricing/"+userID+"/preview") || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/pricing/"+userID+"/publish") || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/pricing/"+userID+"/transition") || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/metering-units/"+userID+"/deactivate") {
		t.Fatal("admin portal must expose versioned metering and pricing lifecycle operations")
	}
	if !billingAdminPathAllowed(http.MethodGet, "/admin/v1/recharge-products") || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/recharge-products") || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/recharge-products/"+userID+"/publish") || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/recharge-products/"+userID+"/unpublish") || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/recharge-products/"+userID+"/delete") || !billingAdminPathAllowed(http.MethodGet, "/admin/v1/recharge-products/"+userID+"/admission-verifications") || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/recharge-products/"+userID+"/admission-verifications/"+memberID+"/retry") || !billingAdminPathAllowed(http.MethodPost, "/admin/v1/recharge-products/"+userID+"/admission-verifications/"+memberID+"/revoke") {
		t.Fatal("admin portal must expose only the recharge product catalog lifecycle")
	}
	if !accountAdminPathAllowed(http.MethodGet, "/admin/v1/approval-policies") || !accountAdminPathAllowed(http.MethodGet, "/admin/v1/approval-requests") || !accountAdminPathAllowed(http.MethodPost, "/admin/v1/approval-requests") || !accountAdminPathAllowed(http.MethodPut, "/admin/v1/approval-requests/"+userID) || !accountAdminPathAllowed(http.MethodPost, "/admin/v1/approval-requests/"+userID+"/actions/decide") {
		t.Fatal("approval workflow paths must be allowlisted")
	}
	if !tokenAdminPathAllowed(http.MethodPost, "/admin/v1/token-accounts") || !tokenAdminPathAllowed(http.MethodGet, "/admin/v1/token-accounts/"+userID) || !tokenAdminPathAllowed(http.MethodPatch, "/admin/v1/token-accounts/"+userID+"/status") || !tokenAdminPathAllowed(http.MethodPut, "/admin/v1/token-accounts/"+userID+"/routes") || !tokenAdminPathAllowed(http.MethodDelete, "/admin/v1/token-accounts/"+userID+"/routes") || !tokenAdminPathAllowed(http.MethodPost, "/admin/v1/token-accounts/"+userID+"/credentials/"+memberID+"/validate") || !tokenAdminPathAllowed(http.MethodGet, "/admin/v1/usage-requests/"+userID) || !tokenAdminPathAllowed(http.MethodPost, "/admin/v1/usage-requests/"+userID+"/retry-reconciliation") {
		t.Fatal("admin portal must expose documented Token Gateway operations")
	}
	if !tokenAdminPathAllowed(http.MethodGet, "/admin/v1/protocols") || !tokenAdminPathAllowed(http.MethodPost, "/admin/v1/protocols") || !tokenAdminPathAllowed(http.MethodPost, "/admin/v1/protocols/"+userID+"/publish") || !tokenAdminPathAllowed(http.MethodPost, "/admin/v1/protocols/"+userID+"/formulas") || !tokenAdminPathAllowed(http.MethodPost, "/admin/v1/formulas/"+userID+"/publish") || !tokenAdminPathAllowed(http.MethodGet, "/admin/v1/formulas/"+userID+"/preview") {
		t.Fatal("admin portal must expose protocol and formula lifecycle operations")
	}
	if accountAdminPathAllowed(http.MethodPost, "/admin/v1/users") || accountAdminPathAllowed(http.MethodDelete, "/admin/v1/users/"+userID) || accountAdminPathAllowed(http.MethodDelete, "/admin/v1/users/not-a-uuid") || accountAdminPathAllowed(http.MethodPost, "/admin/v1/registration-operations/"+userID+"/actions/force-complete") || accountAdminPathAllowed(http.MethodPost, "/admin/v1/organizations/"+organizationID+"/members/"+memberID) || accountAdminPathAllowed(http.MethodGet, "/admin/v1/organizations/"+organizationID+"/ownership") || accountAdminPathAllowed(http.MethodDelete, "/admin/v1/organizations/"+organizationID+"/units/not-a-uuid") || billingAdminPathAllowed(http.MethodGet, "/internal/wallets") || tokenAdminPathAllowed(http.MethodGet, "/v1/chat/completions") {
		t.Fatal("admin portal must not expose undocumented mutations, internal APIs, or the model data plane")
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
