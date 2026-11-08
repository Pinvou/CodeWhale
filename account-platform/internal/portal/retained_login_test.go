package portal

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMatureLoginCarriesLocalePKCENonceAndPreviousLineage(t *testing.T) {
	for _, locale := range []string{"zh-CN", "en", "ja"} {
		t.Run(locale, func(t *testing.T) {
			store := newMemoryStore()
			service := testService(t, store)
			session := validSession(uuid.NewString())
			if err := store.SaveSession(context.Background(), session); err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, "/auth/login?reauthenticate=1&locale="+url.QueryEscape(locale)+"&return_to=%2F%3Fsection%3Dprofile", nil)
			request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
			response := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(response, request)
			if response.Code != 302 {
				t.Fatal("login did not start")
			}
			redirect, err := url.Parse(response.Header().Get("Location"))
			if err != nil {
				t.Fatal(err)
			}
			query := redirect.Query()
			flow, ok := store.flows[query.Get("state")]
			if !ok || flow.PreviousSession != session.ID || flow.LineageRoot != session.ID || query.Get("ui_locales") != locale || query.Get("scope") != "openid profile email" || query.Get("prompt") != "login" || query.Get("nonce") != flow.Nonce || flow.Nonce == "" {
				t.Fatal("mature login lost identity/locale/scope facts")
			}
			digest := sha256.Sum256([]byte(flow.Verifier))
			if query.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(digest[:]) || query.Get("code_challenge_method") != "S256" {
				t.Fatal("PKCE flow not bound")
			}
			for _, cookie := range response.Result().Cookies() {
				if strings.Contains(cookie.Value, flow.Verifier) || strings.Contains(cookie.Value, session.AccessToken) {
					t.Fatal("server proof entered browser cookie")
				}
			}
		})
	}
	store := newMemoryStore()
	service := testService(t, store)
	staticCalls := 0
	handler := service.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { staticCalls++; w.WriteHeader(200) }))
	request := httptest.NewRequest(http.MethodGet, "/?locale=ja", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 302 || staticCalls != 0 {
		t.Fatal("anonymous root bypassed login")
	}
	redirect, _ := url.Parse(response.Header().Get("Location"))
	flow := store.flows[redirect.Query().Get("state")]
	if flow.ReturnTo != "/?locale=ja" {
		t.Fatal("locale return route was lost")
	}
}

func TestRealRedisSignedCallbackAfterLogoutCannotRestoreIdentity(t *testing.T) {
	previous, state := uuid.NewString(), uuid.NewString()
	store := realRedisStore(t, previous, state)
	started, finish := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(finish) }) }
	defer release()
	var reply TokenResponse
	service, key := oidcTestService(t, store, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-finish
		json.NewEncoder(w).Encode(reply)
	}))
	reply = TokenResponse{AccessToken: "test-access-new", RefreshToken: "test-refresh-new", IDToken: testIDToken(t, key, nil), TokenType: "Bearer", ExpiresIn: 60, RefreshExpiresIn: 3600}
	prior := validSession(previous)
	if err := store.SaveSession(context.Background(), prior); err != nil {
		t.Fatal(err)
	}
	flow := LoginFlow{Verifier: "test-verifier", Nonce: "nonce-test", ReturnTo: "/", PreviousSession: previous, LineageRoot: previous, ExpiresAt: time.Now().Add(time.Minute)}
	if err := store.SaveFlow(context.Background(), state, flow); err != nil {
		t.Fatal(err)
	}
	callback := httptest.NewRequest(http.MethodGet, "/auth/callback?state="+state+"&code=test-code", nil)
	callback.AddCookie(&http.Cookie{Name: service.flowCookieName(), Value: state})
	finished := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		service.Handler(nil).ServeHTTP(response, callback)
		finished <- response
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("exchange did not start")
	}
	logout := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(logout, logoutRequest(service, prior))
	if logout.Code != 204 {
		t.Fatal("logout did not commit")
	}
	release()
	select {
	case response := <-finished:
		if response.Code != 409 || !strings.Contains(response.Body.String(), "LOGIN_FLOW_INVALIDATED") {
			t.Fatalf("late callback status %d", response.Code)
		}
		for _, cookie := range response.Result().Cookies() {
			if cookie.Name == service.sessionCookieName() && cookie.MaxAge > 0 {
				t.Fatal("late callback issued authenticated cookie")
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("callback did not finish")
	}
	if _, err := store.GetSession(context.Background(), previous); !errors.Is(err, ErrSessionNotFound) {
		t.Fatal("old identity recreated")
	}
	replay := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(replay, callback.Clone(context.Background()))
	if replay.Code != 400 {
		t.Fatal("one-use signed callback replay accepted")
	}
}
