package portal

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type failingSessionHandoffStore struct {
	*memoryStore
	takeErr        error
	saveSessionErr error
	deleteErr      error
}

func (s *failingSessionHandoffStore) TakeSessionHandoff(ctx context.Context, key string) (SessionHandoff, error) {
	if s.takeErr != nil {
		return SessionHandoff{}, s.takeErr
	}
	return s.memoryStore.TakeSessionHandoff(ctx, key)
}

func (s *failingSessionHandoffStore) SaveSession(ctx context.Context, session Session) error {
	if s.saveSessionErr != nil {
		return s.saveSessionErr
	}
	return s.memoryStore.SaveSession(ctx, session)
}

func (s *failingSessionHandoffStore) DeleteSession(ctx context.Context, id string) error {
	if s.deleteErr != nil {
		err := s.deleteErr
		s.deleteErr = nil
		return err
	}
	return s.memoryStore.DeleteSession(ctx, id)
}

func (s *failingSessionHandoffStore) ReplaceSessionFromHandoff(ctx context.Context, previousID string, session Session, markerExpiresAt time.Time) error {
	if s.saveSessionErr != nil {
		return s.saveSessionErr
	}
	if previousID != "" && s.deleteErr != nil {
		return s.deleteErr
	}
	return s.memoryStore.ReplaceSessionFromHandoff(ctx, previousID, session, markerExpiresAt)
}

func newSessionHandoffTestService(t *testing.T, refreshedUser string) (*Service, *memoryStore) {
	t.Helper()
	store := newMemoryStore()
	account := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/me" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		user := refreshedUser
		if r.Header.Get("Authorization") == "Bearer access" {
			user = "11111111-1111-4111-8111-111111111111"
		}
		if user == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user_id":"` + user + `"}`))
	}))
	t.Cleanup(account.Close)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse token form: %v", err)
		}
		if r.Form.Get("grant_type") == "urn:ietf:params:oauth:grant-type:token-exchange" {
			if r.Form.Get("client_id") != "portal-handoff" || r.Form.Get("client_secret") != "handoff-secret" || r.Form.Get("subject_token") != "access" || r.Form.Get("audience") != "" || r.Form.Get("requested_token_type") != "" || r.Form.Get("scope") != "openid" {
				t.Errorf("unexpected token exchange form: %v", r.Form)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-access","token_type":"Bearer","expires_in":900}`))
	}))
	t.Cleanup(provider.Close)
	accountURL, _ := url.Parse(account.URL)
	tokenURL, _ := url.Parse(provider.URL + "/token")
	publicURL, _ := url.Parse("http://127.0.0.1:8080")
	service, err := NewService(Config{
		PublicURL: publicURL, AccountURL: accountURL, BillingURL: accountURL,
		TokenGatewayURL: accountURL, TokenURL: tokenURL, AuthorizationURL: tokenURL,
		ClientID: "portal", HandoffClientID: "portal-handoff", HandoffClientSecret: "handoff-secret",
		HandoffSubjectTokenEnabled: true, SessionHandoffTTL: 30 * time.Second, SessionLineageLogoutEnabled: true,
		SessionMaxAge: 30 * 24 * time.Hour,
	}, store)
	if err != nil {
		t.Fatal(err)
	}
	return service, store
}

func createSessionHandoffTicket(t *testing.T, service *Service) string {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/portal-session-handoff", http.NoBody)
	request.Header.Set("Authorization", "Bearer access")
	response := httptest.NewRecorder()
	service.createSessionHandoff(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("create status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	return payload.Ticket
}

func consumeSessionHandoffTicket(service *Service, ticket, contentType, origin string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/portal-session-handoff/consume", strings.NewReader(`{"ticket":"`+ticket+`"}`))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	response := httptest.NewRecorder()
	service.consumeSessionHandoff(response, request)
	return response
}

func TestSessionHandoffRoutesReachHandlersThroughProductionMiddleware(t *testing.T) {
	service, store := newSessionHandoffTestService(t, "11111111-1111-4111-8111-111111111111")
	handler := service.Handler(StaticHandler())

	pageRequest := httptest.NewRequest(http.MethodGet, "/portal-session-handoff", http.NoBody)
	pageResponse := httptest.NewRecorder()
	handler.ServeHTTP(pageResponse, pageRequest)
	if pageResponse.Code != http.StatusOK {
		t.Fatalf("page status = %d, body = %s", pageResponse.Code, pageResponse.Body.String())
	}
	if !strings.Contains(pageResponse.Body.String(), "portal-session-handoff.js") {
		t.Fatalf("page did not serve the handoff document: %s", pageResponse.Body.String())
	}
	if got := pageResponse.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("page Cache-Control = %q, want no-cache", got)
	}

	createRequest := httptest.NewRequest(http.MethodPost, "/portal-session-handoff", http.NoBody)
	createRequest.Header.Set("Authorization", "Bearer access")
	createResponse := httptest.NewRecorder()
	handler.ServeHTTP(createResponse, createRequest)
	if createResponse.Code != http.StatusOK {
		t.Fatalf("create status = %d, body = %s", createResponse.Code, createResponse.Body.String())
	}
	var payload struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(createResponse.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}

	consumeRequest := httptest.NewRequest(http.MethodPost, "/portal-session-handoff/consume", strings.NewReader(`{"ticket":"`+payload.Ticket+`"}`))
	consumeRequest.Header.Set("Content-Type", "application/json")
	consumeRequest.Header.Set("Origin", service.config.PublicURL.String())
	consumeResponse := httptest.NewRecorder()
	handler.ServeHTTP(consumeResponse, consumeRequest)
	if consumeResponse.Code != http.StatusOK {
		t.Fatalf("consume status = %d, body = %s", consumeResponse.Code, consumeResponse.Body.String())
	}
	if len(store.sessions) != 1 {
		t.Fatalf("stored sessions = %d, want 1", len(store.sessions))
	}
	replayRequest := httptest.NewRequest(http.MethodPost, "/portal-session-handoff/consume", strings.NewReader(`{"ticket":"`+payload.Ticket+`"}`))
	replayRequest.Header.Set("Content-Type", "application/json")
	replayRequest.Header.Set("Origin", service.config.PublicURL.String())
	replayResponse := httptest.NewRecorder()
	handler.ServeHTTP(replayResponse, replayRequest)
	if replayResponse.Code != http.StatusUnauthorized {
		t.Fatalf("replay status = %d, body = %s", replayResponse.Code, replayResponse.Body.String())
	}
}

func TestSessionHandoffPagePersistsDesktopLocale(t *testing.T) {
	response := httptest.NewRecorder()
	StaticHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/portal-session-handoff.js", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("script status = %d", response.Code)
	}
	helperResponse := httptest.NewRecorder()
	StaticHandler().ServeHTTP(helperResponse, httptest.NewRequest(http.MethodGet, "/assets/portal-session-handoff-url.js", nil))
	if helperResponse.Code != http.StatusOK {
		t.Fatalf("URL helper status = %d", helperResponse.Code)
	}
	script := response.Body.String() + helperResponse.Body.String()
	for _, contract := range []string{
		`new URLSearchParams(location.search).get("locale")`,
		`portalSessionHandoffLanguage(requestedLocale, navigator.language, localStorage)`,
		`storage.setItem('pinvou.account.language', requestedLanguage)`,
		`takePortalSessionHandoffTicket(location.href, history.replaceState.bind(history))`,
		`cleanURL.searchParams.delete('locale')`,
	} {
		if !strings.Contains(script, contract) {
			t.Fatalf("handoff locale contract missing %q", contract)
		}
	}
}

func TestSessionHandoffCreatesSingleUsePortalSession(t *testing.T) {
	service, store := newSessionHandoffTestService(t, "11111111-1111-4111-8111-111111111111")
	request := httptest.NewRequest(http.MethodPost, "/portal-session-handoff", http.NoBody)
	request.Header.Set("Authorization", "Bearer access")
	response := httptest.NewRecorder()
	service.createSessionHandoff(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("create status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	consume := httptest.NewRequest(http.MethodPost, "/portal-session-handoff/consume", strings.NewReader(`{"ticket":"`+payload.Ticket+`"}`))
	consume.Header.Set("Content-Type", "application/json")
	consume.Header.Set("Origin", service.config.PublicURL.String())
	consumeResponse := httptest.NewRecorder()
	service.consumeSessionHandoff(consumeResponse, consume)
	if consumeResponse.Code != http.StatusOK {
		t.Fatalf("consume status = %d", consumeResponse.Code)
	}
	if len(store.sessions) != 1 {
		t.Fatalf("portal session count = %d", len(store.sessions))
	}
	replay := httptest.NewRequest(http.MethodPost, "/portal-session-handoff/consume", strings.NewReader(`{"ticket":"`+payload.Ticket+`"}`))
	replay.Header.Set("Content-Type", "application/json")
	replay.Header.Set("Origin", service.config.PublicURL.String())
	replayResponse := httptest.NewRecorder()
	service.consumeSessionHandoff(replayResponse, replay)
	if replayResponse.Code != http.StatusUnauthorized {
		t.Fatalf("replay status = %d", replayResponse.Code)
	}
}

func TestSessionHandoffSessionNeverOutlivesExchangedAccessToken(t *testing.T) {
	service, _ := newSessionHandoffTestService(t, "11111111-1111-4111-8111-111111111111")
	request := httptest.NewRequest(http.MethodPost, "/portal-session-handoff", http.NoBody)
	request.Header.Set("Authorization", "Bearer access")
	createResponse := httptest.NewRecorder()
	service.createSessionHandoff(createResponse, request)
	if createResponse.Code != http.StatusOK {
		t.Fatalf("create status = %d", createResponse.Code)
	}
	var payload struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(createResponse.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	consume := httptest.NewRequest(http.MethodPost, "/portal-session-handoff/consume", strings.NewReader(`{"ticket":"`+payload.Ticket+`"}`))
	consume.Header.Set("Content-Type", "application/json")
	consume.Header.Set("Origin", service.config.PublicURL.String())
	consumeResponse := httptest.NewRecorder()
	service.consumeSessionHandoff(consumeResponse, consume)
	if consumeResponse.Code != http.StatusOK {
		t.Fatalf("consume status = %d", consumeResponse.Code)
	}
	cookies := map[string]*http.Cookie{}
	for _, cookie := range consumeResponse.Result().Cookies() {
		cookies[cookie.Name] = cookie
	}
	var sessionID string
	for _, cookie := range cookies {
		if strings.HasSuffix(cookie.Name, "portal_session") {
			sessionID = cookie.Value
		}
	}
	session, err := service.store.GetSession(request.Context(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if session.RefreshToken != "" {
		t.Fatal("handoff session must not contain a refresh token")
	}
	if !session.RefreshExpiresAt.Equal(session.AccessExpiresAt) {
		t.Fatalf("session expiry %s does not match access expiry %s", session.RefreshExpiresAt, session.AccessExpiresAt)
	}
	session.AccessExpiresAt = time.Now().Add(20 * time.Second)
	session.RefreshExpiresAt = session.AccessExpiresAt
	if err := service.store.SaveSession(request.Context(), session); err != nil {
		t.Fatal(err)
	}
	statusRequest := httptest.NewRequest(http.MethodGet, "/api/session", http.NoBody)
	statusRequest.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
	if _, err := service.authenticatedSession(statusRequest.Context(), statusRequest); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("near-expiry access-only session was refreshed or extended: %v", err)
	}
}

func TestSessionHandoffUsesPreExchangeClockAndRejectsExpiredExchange(t *testing.T) {
	service, store := newSessionHandoffTestService(t, "11111111-1111-4111-8111-111111111111")
	exchangeStartedAt := time.Date(2026, time.September, 22, 10, 0, 0, 0, time.UTC)
	var clockCalls atomic.Int32
	service.now = func() time.Time {
		if clockCalls.Add(1) == 1 {
			return exchangeStartedAt
		}
		return exchangeStartedAt.Add(901 * time.Second)
	}
	request := httptest.NewRequest(http.MethodPost, "/portal-session-handoff", http.NoBody)
	request.Header.Set("Authorization", "Bearer access")
	response := httptest.NewRecorder()
	service.createSessionHandoff(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if len(store.handoffs) != 0 || len(store.sessions) != 0 {
		t.Fatal("an exchange that expired during verification must not create state")
	}
}

func TestSessionHandoffCapsTicketToExchangedAccessSafetyWindow(t *testing.T) {
	service, store := newSessionHandoffTestService(t, "11111111-1111-4111-8111-111111111111")
	exchangeStartedAt := time.Date(2026, time.September, 22, 10, 0, 0, 0, time.UTC)
	verificationFinishedAt := exchangeStartedAt.Add(850 * time.Second)
	var clockCalls atomic.Int32
	service.now = func() time.Time {
		if clockCalls.Add(1) == 1 {
			return exchangeStartedAt
		}
		return verificationFinishedAt
	}
	request := httptest.NewRequest(http.MethodPost, "/portal-session-handoff", http.NoBody)
	request.Header.Set("Authorization", "Bearer access")
	response := httptest.NewRecorder()
	service.createSessionHandoff(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload struct {
		TicketExpiresIn int `json:"ticket_expires_in"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.TicketExpiresIn != 20 {
		t.Fatalf("ticket_expires_in = %d, want 20", payload.TicketExpiresIn)
	}
	if len(store.handoffs) != 1 {
		t.Fatalf("handoffs = %d, want 1", len(store.handoffs))
	}
	for _, handoff := range store.handoffs {
		wantAccessExpiry := exchangeStartedAt.Add(900 * time.Second)
		if !handoff.Session.AccessExpiresAt.Equal(wantAccessExpiry) || !handoff.Session.RefreshExpiresAt.Equal(wantAccessExpiry) {
			t.Fatalf("session expiry = %s/%s, want %s", handoff.Session.AccessExpiresAt, handoff.Session.RefreshExpiresAt, wantAccessExpiry)
		}
		if want := wantAccessExpiry.Add(-30 * time.Second); !handoff.ExpiresAt.Equal(want) {
			t.Fatalf("handoff expiry = %s, want %s", handoff.ExpiresAt, want)
		}
	}
}

func TestSessionHandoffConsumeRejectsCrossOrigin(t *testing.T) {
	service, store := newSessionHandoffTestService(t, "11111111-1111-4111-8111-111111111111")
	request := httptest.NewRequest(http.MethodPost, "/portal-session-handoff", http.NoBody)
	request.Header.Set("Authorization", "Bearer access")
	createResponse := httptest.NewRecorder()
	service.createSessionHandoff(createResponse, request)
	var payload struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(createResponse.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	consume := httptest.NewRequest(http.MethodPost, "/portal-session-handoff/consume", strings.NewReader(`{"ticket":"`+payload.Ticket+`"}`))
	consume.Header.Set("Content-Type", "application/json")
	consume.Header.Set("Origin", "https://evil.example")
	consumeResponse := httptest.NewRecorder()
	service.consumeSessionHandoff(consumeResponse, consume)
	if consumeResponse.Code != http.StatusForbidden {
		t.Fatalf("status = %d", consumeResponse.Code)
	}
	if len(store.handoffs) != 1 {
		t.Fatal("cross-origin request must not consume ticket")
	}
	pathOrigin := httptest.NewRequest(http.MethodPost, "/portal-session-handoff/consume", strings.NewReader(`{"ticket":"`+payload.Ticket+`"}`))
	pathOrigin.Header.Set("Content-Type", "application/json")
	pathOrigin.Header.Set("Origin", service.config.PublicURL.String()+"/unexpected")
	pathResponse := httptest.NewRecorder()
	service.consumeSessionHandoff(pathResponse, pathOrigin)
	if pathResponse.Code != http.StatusForbidden || len(store.handoffs) != 1 {
		t.Fatalf("path-bearing origin status/state = %d/%d", pathResponse.Code, len(store.handoffs))
	}
}

func TestSessionHandoffTreatsExchangedIdentityMismatchAsDependencyFailure(t *testing.T) {
	service, store := newSessionHandoffTestService(t, "22222222-2222-4222-8222-222222222222")
	request := httptest.NewRequest(http.MethodPost, "/portal-session-handoff", http.NoBody)
	request.Header.Set("Authorization", "Bearer access")
	response := httptest.NewRecorder()
	service.createSessionHandoff(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", response.Code)
	}
	if len(store.handoffs) != 0 || len(store.sessions) != 0 {
		t.Fatal("mismatched identity must not create state")
	}
}

func TestSessionHandoffTreatsExchangedTokenRejectionAsDependencyFailure(t *testing.T) {
	service, store := newSessionHandoffTestService(t, "")
	request := httptest.NewRequest(http.MethodPost, "/portal-session-handoff", http.NoBody)
	request.Header.Set("Authorization", "Bearer access")
	response := httptest.NewRecorder()
	service.createSessionHandoff(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if len(store.handoffs) != 0 || len(store.sessions) != 0 {
		t.Fatal("a rejected exchanged token must not create state")
	}
}

func TestSessionHandoffConsumeStoreUnavailableReturnsServiceUnavailable(t *testing.T) {
	service, store := newSessionHandoffTestService(t, "11111111-1111-4111-8111-111111111111")
	ticket := createSessionHandoffTicket(t, service)
	service.store = &failingSessionHandoffStore{memoryStore: store, takeErr: errors.New("redis unavailable")}

	response := consumeSessionHandoffTicket(service, ticket, "application/json", service.config.PublicURL.String())
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if len(store.handoffs) != 1 || len(store.sessions) != 0 {
		t.Fatal("an unavailable store must not consume or create session state")
	}
}

func TestSessionHandoffConsumeRejectsExpiredTicket(t *testing.T) {
	service, store := newSessionHandoffTestService(t, "11111111-1111-4111-8111-111111111111")
	ticket := createSessionHandoffTicket(t, service)
	for key, handoff := range store.handoffs {
		handoff.ExpiresAt = time.Now().Add(-time.Second)
		store.handoffs[key] = handoff
	}

	response := consumeSessionHandoffTicket(service, ticket, "application/json; charset=utf-8", service.config.PublicURL.String())
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if len(store.handoffs) != 0 || len(store.sessions) != 0 {
		t.Fatal("an expired ticket must be consumed without creating a session")
	}
}

func TestSessionHandoffDisabledAndInvalidContentTypeDoNotConsumeTicket(t *testing.T) {
	service, store := newSessionHandoffTestService(t, "11111111-1111-4111-8111-111111111111")
	ticket := createSessionHandoffTicket(t, service)

	wrongType := consumeSessionHandoffTicket(service, ticket, "text/plain", service.config.PublicURL.String())
	if wrongType.Code != http.StatusUnsupportedMediaType || len(store.handoffs) != 1 {
		t.Fatalf("wrong content type status/state = %d/%d", wrongType.Code, len(store.handoffs))
	}

	service.config.HandoffSubjectTokenEnabled = false
	disabled := consumeSessionHandoffTicket(service, ticket, "application/json", service.config.PublicURL.String())
	if disabled.Code != http.StatusServiceUnavailable || len(store.handoffs) != 1 {
		t.Fatalf("disabled status/state = %d/%d", disabled.Code, len(store.handoffs))
	}
	create := httptest.NewRequest(http.MethodPost, "/portal-session-handoff", http.NoBody)
	create.Header.Set("Authorization", "Bearer access")
	createResponse := httptest.NewRecorder()
	service.createSessionHandoff(createResponse, create)
	if createResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled create status = %d", createResponse.Code)
	}
}

func TestSessionHandoffConsumeReplacesPreviousSessionAndSetsSecureCookies(t *testing.T) {
	service, store := newSessionHandoffTestService(t, "11111111-1111-4111-8111-111111111111")
	service.config.SecureCookies = true
	now := time.Now().UTC()
	previous := Session{ID: "previous-session", AccessToken: "old-access", RefreshToken: "old-refresh", AccessExpiresAt: now.Add(time.Hour), RefreshExpiresAt: now.Add(2 * time.Hour), CSRFToken: strings.Repeat("p", 32)}
	if err := store.SaveSession(context.Background(), previous); err != nil {
		t.Fatal(err)
	}
	store.logoutMarkers[previous.ID] = time.Now().Add(time.Hour)
	ticket := createSessionHandoffTicket(t, service)
	request := httptest.NewRequest(http.MethodPost, "/portal-session-handoff/consume", strings.NewReader(`{"ticket":"`+ticket+`"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", service.config.PublicURL.String())
	request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: previous.ID})
	response := httptest.NewRecorder()
	service.consumeSessionHandoff(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if _, err := store.GetSession(context.Background(), previous.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("previous session still exists: %v", err)
	}
	if len(store.sessions) != 1 {
		t.Fatalf("session count = %d", len(store.sessions))
	}
	if _, exists := store.logoutMarkers[previous.ID]; !exists {
		t.Fatal("desktop handoff removed the previous lineage tombstone")
	}
	lateA := Session{ID: "late-a", LineageRoot: previous.ID, RefreshExpiresAt: now.Add(time.Hour)}
	if err := store.CommitLoginSession(context.Background(), previous.ID, lateA); !errors.Is(err, ErrLoginFlowInvalidated) {
		t.Fatalf("old A callback after handoff error = %v", err)
	}
	if len(store.sessions) != 1 {
		t.Fatalf("old A callback changed B session count = %d", len(store.sessions))
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 3 {
		t.Fatalf("cookie count = %d", len(cookies))
	}
	for _, cookie := range cookies {
		if !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" {
			t.Fatalf("cookie attributes = %+v", cookie)
		}
		if strings.HasSuffix(cookie.Name, "portal_session") && !cookie.HttpOnly {
			t.Fatal("portal session cookie must be HttpOnly")
		}
		if strings.HasSuffix(cookie.Name, "portal_csrf") && cookie.HttpOnly {
			t.Fatal("CSRF cookie must remain readable")
		}
		if strings.HasSuffix(cookie.Name, "portal_logged_out") && cookie.MaxAge != -1 {
			t.Fatal("session handoff must clear the explicit logout marker")
		}
	}
}

func TestSessionHandoffInvalidatesOlderFlowWithoutPriorLogout(t *testing.T) {
	store := newMemoryStore()
	now := time.Now().UTC()
	accountA := Session{ID: "account-a", RefreshExpiresAt: now.Add(time.Hour)}
	accountB := Session{ID: "account-b", LineageRoot: "account-b", RefreshExpiresAt: now.Add(time.Hour)}
	if err := store.SaveSession(context.Background(), accountA); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceSessionFromHandoff(context.Background(), accountA.ID, accountB, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	lateA := Session{ID: "late-account-a", LineageRoot: accountA.ID, RefreshExpiresAt: now.Add(time.Hour)}
	if err := store.CommitLoginSession(context.Background(), accountA.ID, lateA); !errors.Is(err, ErrLoginFlowInvalidated) {
		t.Fatalf("old A callback error = %v", err)
	}
	if len(store.sessions) != 1 {
		t.Fatalf("session count = %d", len(store.sessions))
	}
	if _, err := store.GetSession(context.Background(), accountB.ID); err != nil {
		t.Fatalf("desktop B session missing: %v", err)
	}
}

func TestSessionHandoffSaveSessionFailureConsumesTicketWithoutPartialSession(t *testing.T) {
	service, store := newSessionHandoffTestService(t, "11111111-1111-4111-8111-111111111111")
	service.config.SessionLineageLogoutEnabled = false
	ticket := createSessionHandoffTicket(t, service)
	service.store = &failingSessionHandoffStore{memoryStore: store, saveSessionErr: errors.New("save failed")}

	response := consumeSessionHandoffTicket(service, ticket, "application/json", service.config.PublicURL.String())
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if len(store.handoffs) != 0 || len(store.sessions) != 0 {
		t.Fatal("a failed session save must leave no replayable or partial session")
	}
}

func TestCapabilityPhaseHandoffDeleteFailureCompensatesNewSession(t *testing.T) {
	service, store := newSessionHandoffTestService(t, "11111111-1111-4111-8111-111111111111")
	service.config.SessionLineageLogoutEnabled = false
	now := time.Now().UTC()
	previous := Session{ID: "previous", RefreshExpiresAt: now.Add(time.Hour)}
	if err := store.SaveSession(context.Background(), previous); err != nil {
		t.Fatal(err)
	}
	ticket := createSessionHandoffTicket(t, service)
	service.store = &failingSessionHandoffStore{memoryStore: store, deleteErr: errors.New("delete failed")}
	request := httptest.NewRequest(http.MethodPost, "/portal-session-handoff/consume", strings.NewReader(`{"ticket":"`+ticket+`"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", service.config.PublicURL.String())
	request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: previous.ID})
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if len(store.handoffs) != 0 {
		t.Fatal("failed replacement left a replayable ticket")
	}
	if len(store.sessions) != 1 {
		t.Fatalf("compensation left B orphan: %#v", store.sessions)
	}
	if _, err := store.GetSession(context.Background(), previous.ID); err != nil {
		t.Fatalf("previous A session was not preserved: %v", err)
	}
}

func TestSessionHandoffConfigurationRejectsUnsafeTTL(t *testing.T) {
	service, _ := newSessionHandoffTestService(t, "11111111-1111-4111-8111-111111111111")
	for _, ttl := range []time.Duration{0, time.Minute + time.Nanosecond} {
		config := service.config
		config.SessionHandoffTTL = ttl
		_, err := NewService(config, newMemoryStore())
		if err == nil {
			t.Fatalf("TTL %s was accepted", ttl)
		}
	}
}

func TestSessionHandoffConfigurationRequiresDedicatedConfidentialClient(t *testing.T) {
	service, _ := newSessionHandoffTestService(t, "11111111-1111-4111-8111-111111111111")
	for _, mutate := range []func(*Config){
		func(config *Config) { config.HandoffClientID = "" },
		func(config *Config) { config.HandoffClientSecret = "" },
		func(config *Config) { config.HandoffClientID = config.ClientID },
	} {
		config := service.config
		mutate(&config)
		if _, err := NewService(config, newMemoryStore()); err == nil {
			t.Fatal("unsafe handoff client configuration was accepted")
		}
	}
}

func TestSessionHandoffDistinguishesAuthenticationRejectionFromDependencyFailure(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     int
		body       string
		expected   int
		accountAPI bool
	}{
		{name: "account rejects desktop token", status: http.StatusUnauthorized, expected: http.StatusUnauthorized, accountAPI: true},
		{name: "account forbids desktop token", status: http.StatusForbidden, expected: http.StatusServiceUnavailable, accountAPI: true},
		{name: "account unavailable", status: http.StatusServiceUnavailable, expected: http.StatusServiceUnavailable, accountAPI: true},
		{name: "identity unavailable", status: http.StatusServiceUnavailable, body: `{}`, expected: http.StatusServiceUnavailable},
		{name: "handoff client authentication rejected", status: http.StatusUnauthorized, body: `{"error":"invalid_client"}`, expected: http.StatusServiceUnavailable},
		{name: "desktop token rejected", status: http.StatusBadRequest, body: `{"error":"invalid_grant"}`, expected: http.StatusUnauthorized},
		{name: "token exchange misconfigured", status: http.StatusBadRequest, body: `{"error":"invalid_request"}`, expected: http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, store := newSessionHandoffTestService(t, "11111111-1111-4111-8111-111111111111")
			dependency := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			t.Cleanup(dependency.Close)
			endpoint, err := url.Parse(dependency.URL)
			if err != nil {
				t.Fatal(err)
			}
			if test.accountAPI {
				service.config.AccountURL = endpoint
			} else {
				service.config.TokenURL = endpoint
			}
			request := httptest.NewRequest(http.MethodPost, "/portal-session-handoff", http.NoBody)
			request.Header.Set("Authorization", "Bearer access")
			response := httptest.NewRecorder()
			service.createSessionHandoff(response, request)
			if response.Code != test.expected {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			if len(store.handoffs) != 0 || len(store.sessions) != 0 {
				t.Fatal("a rejected or unavailable dependency must not create state")
			}
		})
	}
}

func TestSessionHandoffRejectsNonAccountSubjectBeforeConfidentialExchange(t *testing.T) {
	service, store := newSessionHandoffTestService(t, "11111111-1111-4111-8111-111111111111")
	rejectingAccount := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(rejectingAccount.Close)
	var exchangeCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		exchangeCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(provider.Close)
	service.config.AccountURL, _ = url.Parse(rejectingAccount.URL)
	service.config.TokenURL, _ = url.Parse(provider.URL)

	request := httptest.NewRequest(http.MethodPost, "/portal-session-handoff", http.NoBody)
	request.Header.Set("Authorization", "Bearer provisioner-or-other-subject")
	response := httptest.NewRecorder()
	service.createSessionHandoff(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if exchangeCalls.Load() != 0 {
		t.Fatalf("confidential exchange was called %d times for a rejected subject", exchangeCalls.Load())
	}
	if len(store.handoffs) != 0 || len(store.sessions) != 0 {
		t.Fatal("a rejected non-Account subject created state")
	}
}
