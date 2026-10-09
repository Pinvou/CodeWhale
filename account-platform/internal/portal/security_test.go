package portal

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func parseTestURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	value, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func testIDToken(t *testing.T, key *rsa.PrivateKey, change func(map[string]any)) string {
	t.Helper()
	claims := map[string]any{"iss": "https://public-identity.example/realm", "aud": "portal", "sub": "user-test", "nonce": "nonce-test", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix()}
	if change != nil {
		change(claims)
	}
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"test-key"}`))
	unsigned := header + "." + base64.RawURLEncoding.EncodeToString(body)
	hash := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hash[:])
	if err != nil {
		t.Fatal(err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func oidcTestService(t *testing.T, store Store, tokenHandler http.Handler) (*Service, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "test-key", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB"}}})
	}))
	t.Cleanup(keys.Close)
	endpoint := httptest.NewServer(tokenHandler)
	t.Cleanup(endpoint.Close)
	service, err := NewService(Config{Environment: "test", PublicURL: parseTestURL(t, "https://portal.example"), IssuerURL: parseTestURL(t, "https://public-identity.example/realm"), JWKSURL: parseTestURL(t, keys.URL), AuthorizationURL: parseTestURL(t, "https://public-identity.example/auth"), TokenURL: parseTestURL(t, endpoint.URL), ClientID: "portal", AccountURL: parseTestURL(t, "https://account.example"), TokenGatewayURL: parseTestURL(t, "https://token.example"), BillingURL: parseTestURL(t, "https://billing.example"), SecureCookies: true}, store)
	if err != nil {
		t.Fatal(err)
	}
	return service, key
}

func callbackRequest(t *testing.T, service *Service, store Store) *http.Request {
	t.Helper()
	state := uuid.NewString()
	if err := store.SaveFlow(context.Background(), state, LoginFlow{Verifier: "test-verifier", Nonce: "nonce-test", ReturnTo: "/", ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/auth/callback?state="+state+"&code=test-code", nil)
	request.AddCookie(&http.Cookie{Name: service.flowCookieName(), Value: state})
	return request
}

func TestCallbackVerifiesSignedIdentityNonceAndOneTimeFlow(t *testing.T) {
	for _, name := range []string{"valid", "valid-access-hash", "valid-multiple-audience", "missing-id-token", "wrong-nonce", "wrong-issuer", "wrong-audience", "expired", "wrong-signature", "wrong-authorized-party", "wrong-access-hash", "multiple-audience-missing-authorized-party"} {
		t.Run(name, func(t *testing.T) {
			store := newMemoryStore()
			var reply TokenResponse
			var calls atomic.Int32
			formResult := make(chan url.Values, 1)
			service, key := oidcTestService(t, store, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				r.ParseForm()
				formResult <- r.PostForm
				json.NewEncoder(w).Encode(reply)
			}))
			reply = TokenResponse{AccessToken: "test-access-secret", RefreshToken: "test-refresh-secret", TokenType: "Bearer", ExpiresIn: 60, RefreshExpiresIn: 3600}
			if name != "missing-id-token" {
				if name == "wrong-signature" {
					var err error
					key, err = rsa.GenerateKey(rand.Reader, 2048)
					if err != nil {
						t.Fatal(err)
					}
				}
				reply.IDToken = testIDToken(t, key, func(claims map[string]any) {
					switch name {
					case "wrong-nonce":
						claims["nonce"] = "another-flow"
					case "wrong-issuer":
						claims["iss"] = "https://wrong.example"
					case "wrong-audience":
						claims["aud"] = "another-client"
					case "expired":
						claims["exp"] = time.Now().Add(-time.Minute).Unix()
					case "wrong-authorized-party":
						claims["azp"] = "another-client"
					case "valid-multiple-audience", "multiple-audience-missing-authorized-party":
						claims["aud"] = []string{"portal", "another-audience"}
						if name == "valid-multiple-audience" {
							claims["azp"] = "portal"
						}
					case "valid-access-hash", "wrong-access-hash":
						access := reply.AccessToken
						if name == "wrong-access-hash" {
							access = "another-access-token"
						}
						hash := sha256.Sum256([]byte(access))
						claims["at_hash"] = base64.RawURLEncoding.EncodeToString(hash[:16])
					}
				})
			}
			request := callbackRequest(t, service, store)
			response := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(response, request)
			form := <-formResult
			if form.Get("grant_type") != "authorization_code" || form.Get("code_verifier") != "test-verifier" || form.Get("redirect_uri") != "https://portal.example/auth/callback" {
				t.Fatalf("code exchange did not bind the original flow: %v", form)
			}
			if strings.HasPrefix(name, "valid") {
				if response.Code != http.StatusFound || len(store.sessions) != 1 {
					t.Fatalf("valid identity rejected: %d %s", response.Code, response.Body.String())
				}
				for _, session := range store.sessions {
					if session.Subject != "user-test" || session.Nonce != "nonce-test" {
						t.Fatalf("identity not bound to server session: %+v", session)
					}
				}
				for _, cookie := range response.Result().Cookies() {
					if !cookie.Secure || cookie.Path != "/" || cookie.Domain != "" || cookie.SameSite != http.SameSiteLaxMode || cookie.Name == service.sessionCookieName() && !cookie.HttpOnly {
						t.Fatalf("unsafe cookie metadata: %+v", cookie)
					}
				}
				if strings.Contains(response.Header().Get("Set-Cookie"), "test-access-secret") || strings.Contains(response.Header().Get("Set-Cookie"), "test-refresh-secret") {
					t.Fatal("OIDC tokens entered browser cookies")
				}
			} else if response.Code != http.StatusBadGateway || len(store.sessions) != 0 {
				t.Fatalf("invalid identity created a session: %d %s", response.Code, response.Body.String())
			}
			replay := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(replay, request.Clone(context.Background()))
			if replay.Code != http.StatusBadRequest || calls.Load() != 1 {
				t.Fatalf("consumed flow replayed: status %d exchanges %d", replay.Code, calls.Load())
			}
		})
	}
}

func TestReturnToRejectsBrowserNormalizedExternalPaths(t *testing.T) {
	for _, value := range []string{"//evil.example", `/\evil.example`, `/\\evil.example`, "/a\tb", "/a\x00b", "https://evil.example"} {
		if safeReturnTo(value) != "/" {
			t.Fatalf("unsafe browser return path accepted: %q", value)
		}
	}
	if safeReturnTo("/wallet?view=recent#order") != "/wallet?view=recent#order" {
		t.Fatal("safe local return target changed")
	}
}

func TestAuthenticationAndAPIResponsesAreNotCacheable(t *testing.T) {
	service := testService(t, newMemoryStore())
	for _, path := range []string{"/auth/login", "/auth/callback", "/api/session", "/api/token/v1/me/balance"} {
		response := httptest.NewRecorder()
		service.Handler(nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("authentication response %s can be cached", path)
		}
	}
}

type deleteFailureStore struct{ *memoryStore }

func (*deleteFailureStore) LogoutSession(context.Context, string, string, time.Time) error {
	return errors.New("test-only Redis delete failure")
}

func validSession(id string) Session {
	return Session{ID: id, LineageRoot: id, AccessToken: "test-access", RefreshToken: "test-refresh", AccessExpiresAt: time.Now().UTC().Add(time.Hour), RefreshExpiresAt: time.Now().UTC().Add(2 * time.Hour), CSRFToken: strings.Repeat("c", 32), Subject: "user-test", Nonce: "nonce-test", CreatedAt: time.Now().UTC()}
}

func logoutRequest(service *Service, session Session) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/api/logout", nil)
	request.Header.Set("Origin", "https://portal.example")
	request.Header.Set("X-CSRF-Token", session.CSRFToken)
	request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
	request.AddCookie(&http.Cookie{Name: service.csrfCookieName(), Value: session.CSRFToken})
	return request
}

func TestLogoutDeleteFailureIsNotSuccessfulAndDoesNotRefresh(t *testing.T) {
	store := &deleteFailureStore{newMemoryStore()}
	service := testService(t, store)
	session := validSession(uuid.NewString())
	session.AccessExpiresAt = time.Now().Add(-time.Minute)
	store.SaveSession(context.Background(), session)
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, logoutRequest(service, session))
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Set-Cookie") != "" {
		t.Fatalf("failed logout claimed success or lost retry cookies: %d %s", response.Code, response.Body.String())
	}
	if _, err := store.GetSession(context.Background(), session.ID); err != nil {
		t.Fatal("failed logout removed the session")
	}
}

func TestPortalNeverFollowsOAuthOrManagementRedirects(t *testing.T) {
	var leaked atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Add(1)
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	store := newMemoryStore()
	service := testService(t, store)
	service.config.TokenURL = parseTestURL(t, redirect.URL)
	if _, err := service.exchange(context.Background(), url.Values{"refresh_token": {"test-refresh-secret"}}); err == nil {
		t.Fatal("redirected OAuth exchange succeeded")
	}
	service.config.AccountURL = parseTestURL(t, redirect.URL)
	session := validSession(uuid.NewString())
	store.SaveSession(context.Background(), session)
	request := httptest.NewRequest(http.MethodGet, "/api/account/v1/me", nil)
	request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
	service.Handler(nil).ServeHTTP(httptest.NewRecorder(), request)
	if leaked.Load() != 0 {
		t.Fatal("portal sent token facts to a redirect destination")
	}
}

func TestPortalRejectsTruncatedOversizedRequestsBeforeUpstream(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(http.StatusOK) }))
	defer upstream.Close()
	store := newMemoryStore()
	service := testService(t, store)
	service.config.AccountURL = parseTestURL(t, upstream.URL)
	session := validSession(uuid.NewString())
	store.SaveSession(context.Background(), session)
	payload := append([]byte(`{}`), bytes.Repeat([]byte(" "), (4<<20)-2)...)
	payload = append(payload, []byte(`{"extra":true}`)...)
	request := logoutRequest(service, session)
	request.URL.Path = "/api/account/v1/me/profile-change-requests"
	request.Body = io.NopCloser(bytes.NewReader(payload))
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge || calls.Load() != 0 {
		t.Fatalf("oversized prefix executed upstream: %d calls %d", response.Code, calls.Load())
	}
}

func TestPortalProductionRejectsInsecureOIDCEndpoints(t *testing.T) {
	service := testService(t, newMemoryStore())
	for _, name := range []string{"authorization", "token", "issuer", "keys"} {
		t.Run(name, func(t *testing.T) {
			config := service.config
			config.Environment = "production"
			insecure := parseTestURL(t, "http://identity.example/test")
			switch name {
			case "authorization":
				config.AuthorizationURL = insecure
			case "token":
				config.TokenURL = insecure
			case "issuer":
				config.IssuerURL = insecure
			case "keys":
				config.JWKSURL = insecure
			}
			if _, err := NewService(config, newMemoryStore()); err == nil {
				t.Fatal("production OIDC allowed plaintext token/identity transport")
			}
		})
	}
}

func TestRejectedCSRFDoesNotRefreshOrInvokeManagement(t *testing.T) {
	var calls atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(http.StatusBadRequest) }))
	defer endpoint.Close()
	store := newMemoryStore()
	service := testService(t, store)
	service.config.TokenURL = parseTestURL(t, endpoint.URL)
	service.config.AccountURL = parseTestURL(t, endpoint.URL)
	session := validSession(uuid.NewString())
	session.AccessExpiresAt = time.Now().Add(-time.Minute)
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	request := logoutRequest(service, session)
	request.URL.Path = "/api/account/v1/me/profile-change-requests"
	request.Header.Set("Origin", "https://other.example")
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || calls.Load() != 0 {
		t.Fatalf("invalid mutation caused external side effects %d calls %d", response.Code, calls.Load())
	}
	if _, err := store.GetSession(context.Background(), session.ID); err != nil {
		t.Fatal("invalid mutation destroyed current session")
	}
}

func TestRefreshIdentityBindsOriginalSubjectAndNonce(t *testing.T) {
	for _, name := range []string{"valid-with-nonce", "valid-omitted-nonce", "wrong-subject", "wrong-nonce", "invalid-access-response"} {
		t.Run(name, func(t *testing.T) {
			store := newMemoryStore()
			var reply TokenResponse
			service, key := oidcTestService(t, store, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(reply) }))
			reply = TokenResponse{AccessToken: "test-new-access", RefreshToken: "test-new-refresh", TokenType: "Bearer", ExpiresIn: 60, RefreshExpiresIn: 3600}
			reply.IDToken = testIDToken(t, key, func(claims map[string]any) {
				switch name {
				case "valid-omitted-nonce":
					delete(claims, "nonce")
				case "wrong-subject":
					claims["sub"] = "another-user"
				case "wrong-nonce":
					claims["nonce"] = "another-flow"
				}
			})
			if name == "invalid-access-response" {
				reply.AccessToken = ""
			}
			session := validSession(uuid.NewString())
			session.AccessExpiresAt = time.Now().UTC().Add(-time.Minute)
			store.SaveSession(context.Background(), session)
			refreshed, err := service.refreshSession(context.Background(), session)
			if strings.HasPrefix(name, "valid-") {
				if err != nil || refreshed.Subject != session.Subject || refreshed.RefreshToken != "test-new-refresh" {
					t.Fatalf("valid refresh failed %+v %v", refreshed, err)
				}
			} else if err == nil {
				t.Fatal("invalid refresh identity or access response accepted")
			}
		})
	}
}

type readFailureStore struct{ *memoryStore }

func (*readFailureStore) GetSession(context.Context, string) (Session, error) {
	return Session{}, errors.New("test-only Redis connection unavailable")
}

func TestStoreReadFailureIs503AcrossSessionLogoutAndProxy(t *testing.T) {
	store := &readFailureStore{newMemoryStore()}
	service := testService(t, store)
	session := validSession(uuid.NewString())
	for _, route := range []string{"/api/session", "/api/logout", "/api/token/v1/me/balance", "/api/account/v1/me"} {
		request := logoutRequest(service, session)
		request.URL.Path = route
		if route != "/api/logout" {
			request.Method = http.MethodGet
		}
		response := httptest.NewRecorder()
		service.Handler(nil).ServeHTTP(response, request)
		if response.Code != http.StatusServiceUnavailable || response.Header().Get("Set-Cookie") != "" {
			t.Fatalf("store outage masqueraded as logout/CSRF failure: %s %d %s", route, response.Code, response.Body.String())
		}
	}
}

func TestExpiredAPIIsUnauthorizedButProvenLogoutIsIdempotent(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	session := validSession(uuid.NewString())
	session.RefreshExpiresAt = time.Now().Add(-time.Minute)
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/token/v1/me/balance", nil)
	request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("expired API status %d", response.Code)
	}
	for i := 0; i < 2; i++ {
		response = httptest.NewRecorder()
		service.Handler(nil).ServeHTTP(response, logoutRequest(service, session))
		if response.Code != http.StatusNoContent {
			t.Fatalf("proven logout retry status %d", response.Code)
		}
	}
	next := validSession(uuid.NewString())
	if err := store.SaveSession(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	request = logoutRequest(service, next)
	request.Header.Set("X-CSRF-Token", "wrong")
	response = httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("invalid CSRF status %d", response.Code)
	}
	if _, err := store.GetSession(context.Background(), next.ID); err != nil {
		t.Fatal("invalid CSRF removed live session")
	}
}

func TestRefreshFailureDistinguishesUnavailableFromInvalidGrant(t *testing.T) {
	for _, mode := range []string{"unavailable", "invalid-grant", "lock-conflict"} {
		t.Run(mode, func(t *testing.T) {
			store := newMemoryStore()
			service, _ := oidcTestService(t, store, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "invalid-grant" {
					w.WriteHeader(http.StatusBadRequest)
					io.WriteString(w, `{"error":"invalid_grant"}`)
					return
				}
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			session := validSession(uuid.NewString())
			session.AccessExpiresAt = time.Now().Add(-time.Minute)
			store.SaveSession(context.Background(), session)
			if mode == "lock-conflict" {
				store.TryRefreshLock(context.Background(), session.ID, "another-owner")
			}
			handler := service.Handler(nil)
			// Classify the provider response without charging route setup to the request deadline.
			// Only the lock-conflict case depends on a short request timeout.
			timeout := 2 * time.Second
			if mode == "lock-conflict" {
				timeout = 50 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			request := httptest.NewRequest(http.MethodGet, "/api/session", nil).WithContext(ctx)
			request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			expected := http.StatusServiceUnavailable
			if mode == "invalid-grant" {
				expected = http.StatusUnauthorized
			}
			if response.Code != expected || response.Header().Get("Set-Cookie") != "" {
				t.Fatalf("refresh failure status %d expected %d %s", response.Code, expected, response.Body.String())
			}
			_, err := store.GetSession(context.Background(), session.ID)
			if mode == "invalid-grant" {
				if !errors.Is(err, ErrSessionNotFound) {
					t.Fatal("rejected refresh grant survived")
				}
			} else if err != nil {
				t.Fatal("temporarily unavailable refresh destroyed session")
			}
		})
	}
}

func TestRefreshCookiesUseOnlyProviderReturnedExpiry(t *testing.T) {
	store := newMemoryStore()
	service, _ := oidcTestService(t, store, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(TokenResponse{AccessToken: "test-new-access", RefreshToken: "test-new-refresh", TokenType: "Bearer", ExpiresIn: 60, RefreshExpiresIn: 600})
	}))
	session := validSession(uuid.NewString())
	session.AccessExpiresAt = time.Now().Add(-time.Minute)
	session.RefreshExpiresAt = time.Now().Add(time.Minute)
	store.SaveSession(context.Background(), session)
	request := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)
	latest, err := store.GetSession(context.Background(), session.ID)
	if response.Code != http.StatusOK || err != nil {
		t.Fatalf("refresh failed %d %v", response.Code, err)
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 2 {
		t.Fatalf("expected session/CSRF expiry synchronization, got %d cookies", len(cookies))
	}
	for _, cookie := range cookies {
		if !cookie.Expires.Equal(latest.RefreshExpiresAt.Truncate(time.Second)) || cookie.MaxAge < 598 || cookie.MaxAge > 600 {
			t.Fatalf("cookie expiry does not match authoritative refresh expiry: %+v", cookie)
		}
	}
}

func TestOnlineRefreshRejectsMissingZeroAndNegativeProviderLifetime(t *testing.T) {
	for _, mode := range []string{"missing", "zero", "negative"} {
		t.Run(mode, func(t *testing.T) {
			store := newMemoryStore()
			service, _ := oidcTestService(t, store, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				response := map[string]any{"access_token": "test-new-access", "refresh_token": "test-new-refresh", "token_type": "Bearer", "expires_in": 60}
				if mode == "zero" {
					response["refresh_expires_in"] = 0
				}
				if mode == "negative" {
					response["refresh_expires_in"] = -1
				}
				json.NewEncoder(w).Encode(response)
			}))
			session := validSession(uuid.NewString())
			session.AccessExpiresAt = time.Now().Add(-time.Minute)
			store.SaveSession(context.Background(), session)
			request := httptest.NewRequest(http.MethodGet, "/api/session", nil)
			request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
			response := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(response, request)
			latest, err := store.GetSession(context.Background(), session.ID)
			if response.Code != http.StatusServiceUnavailable || response.Header().Get("Set-Cookie") != "" || err != nil || latest != session {
				t.Fatalf("invalid provider lifetime was accepted or old session changed: %s %d %v", mode, response.Code, err)
			}
		})
	}
}

type flowFailureStore struct{ *memoryStore }

func (*flowFailureStore) TakeFlow(context.Context, string) (LoginFlow, error) {
	return LoginFlow{}, errors.New("test-only Redis transport failure")
}

func TestCallbackStoreFailureIsUnavailableAndMissingFlowIsInvalid(t *testing.T) {
	for _, fail := range []bool{true, false} {
		var store Store = newMemoryStore()
		if fail {
			store = &flowFailureStore{newMemoryStore()}
		}
		service := testService(t, store)
		state := uuid.NewString()
		request := httptest.NewRequest(http.MethodGet, "/auth/callback?state="+state+"&code=test-code", nil)
		request.AddCookie(&http.Cookie{Name: service.flowCookieName(), Value: state})
		response := httptest.NewRecorder()
		service.Handler(nil).ServeHTTP(response, request)
		expected := http.StatusBadRequest
		if fail {
			expected = http.StatusServiceUnavailable
		}
		if response.Code != expected || response.Header().Get("Set-Cookie") != "" {
			t.Fatalf("callback flow failure misclassified: failure=%t status=%d", fail, response.Code)
		}
	}
}
