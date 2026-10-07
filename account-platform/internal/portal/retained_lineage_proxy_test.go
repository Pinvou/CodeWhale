package portal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRealRedisLineageLogoutFencesAllGenerationsAndPendingFlow(t *testing.T) {
	ctx := context.Background()
	original, first, second, pending := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	store := realRedisStore(t, original, first, second, pending)
	a := validSession(original)
	a.RefreshExpiresAt = time.Now().Add(2 * time.Hour)
	if err := store.SaveSession(ctx, a); err != nil {
		t.Fatal(err)
	}
	b := validSession(first)
	b.LineageRoot = original
	b.RefreshExpiresAt = time.Now().Add(time.Hour)
	if err := store.CommitLoginSession(ctx, original, b); err != nil {
		t.Fatal(err)
	}
	c := validSession(second)
	c.LineageRoot = original
	c.RefreshExpiresAt = time.Now().Add(30 * time.Minute)
	if err := store.CommitLoginSession(ctx, original, c); err != nil {
		t.Fatal(err)
	}
	ttl, err := store.client.PTTL(ctx, lineageMembersKey(original)).Result()
	if err != nil || ttl < 119*time.Minute {
		t.Fatalf("later short session shortened lineage %s", ttl)
	}
	if root, err := store.ResolveSessionLineageRoot(ctx, original); err != nil || root != original {
		t.Fatal("stale cookie lost authoritative root")
	}
	if err := store.LogoutSession(ctx, original, "", time.Now().Add(loginFlowLifetime)); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{original, first, second} {
		if _, err := store.GetSession(ctx, id); !errors.Is(err, ErrSessionNotFound) {
			t.Fatal("lineage logout left an authenticated generation")
		}
	}
	ttl, err = store.client.PTTL(ctx, logoutMarkerKey(original)).Result()
	if err != nil || ttl < 119*time.Minute {
		t.Fatalf("marker did not cover actual provider lifetime %s", ttl)
	}
	next := validSession(pending)
	next.LineageRoot = original
	if err := store.CommitLoginSession(ctx, original, next); !errors.Is(err, ErrLoginFlowInvalidated) {
		t.Fatal("pre-logout callback recreated session")
	}
	if err := store.SaveSession(ctx, b); !errors.Is(err, ErrLoginFlowInvalidated) {
		t.Fatal("late session save recreated lineage")
	}
	if err := store.LogoutSession(ctx, second, "", time.Now().Add(loginFlowLifetime)); err != nil {
		t.Fatal(err)
	}
	ttl, err = store.client.PTTL(ctx, logoutMarkerKey(original)).Result()
	if err != nil || ttl < 119*time.Minute {
		t.Fatal("idempotent logout shortened the marker")
	}
}

func TestRealRedisHandoffReplacementIsAtomicAndTicketSingleUse(t *testing.T) {
	ctx := context.Background()
	oldID, newID, ticketID, pending := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	store := realRedisStore(t, oldID, newID, ticketID, pending)
	a := validSession(oldID)
	a.RefreshExpiresAt = time.Now().Add(2 * time.Hour)
	if err := store.SaveSession(ctx, a); err != nil {
		t.Fatal(err)
	}
	b := validSession(newID)
	b.AccessExpiresAt = time.Now().Add(5 * time.Minute)
	b.RefreshExpiresAt = b.AccessExpiresAt
	b.RefreshToken = ""
	b.OIDCClientID = "handoff"
	b.Subject = ""
	b.Nonce = ""
	handoff := SessionHandoff{Session: b, ExpiresAt: time.Now().Add(time.Minute)}
	if err := store.SaveSessionHandoff(ctx, ticketID, handoff); err != nil {
		t.Fatal(err)
	}
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := store.TakeSessionHandoff(ctx, ticketID)
			if err == nil {
				successes.Add(1)
				if got.Session.ID != newID {
					t.Error("ticket changed session")
				}
			} else if !errors.Is(err, ErrSessionNotFound) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatal("handoff consumed more than once")
	}
	if err := store.ReplaceSessionFromHandoff(ctx, oldID, b, time.Now().Add(loginFlowLifetime)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetSession(ctx, oldID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatal("handoff retained previous identity")
	}
	if got, err := store.GetSession(ctx, newID); err != nil || got.AccessToken != b.AccessToken || got.LineageRoot != newID {
		t.Fatal("replacement missing or lineage mixed")
	}
	ttl, err := store.client.PTTL(ctx, logoutMarkerKey(oldID)).Result()
	if err != nil || ttl < 119*time.Minute {
		t.Fatal("replacement did not fence actual old lifetime")
	}
	next := validSession(pending)
	next.LineageRoot = oldID
	if err := store.CommitLoginSession(ctx, oldID, next); !errors.Is(err, ErrLoginFlowInvalidated) {
		t.Fatal("older callback superseded handoff")
	}
	if err := store.LogoutSession(ctx, oldID, "", time.Now().Add(loginFlowLifetime)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetSession(ctx, newID); err != nil {
		t.Fatal("stale old logout removed new desktop identity")
	}
}

func TestRealRedisHandoffThroughProductionHTTPReplacesAndCannotReplay(t *testing.T) {
	oldID := uuid.NewString()
	store := realRedisStore(t, oldID)
	service, _ := newSessionHandoffTestService(t, "11111111-1111-4111-8111-111111111111")
	service.store = store
	old := validSession(oldID)
	if err := store.SaveSession(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	ticket := createSessionHandoffTicket(t, service)
	digest := sha256.Sum256([]byte(ticket))
	key := hex.EncodeToString(digest[:])
	t.Cleanup(func() { _ = store.client.Del(context.Background(), "portal:session-handoff:"+key).Err() })
	request := httptest.NewRequest(http.MethodPost, "/portal-session-handoff/consume", strings.NewReader(`{"ticket":"`+ticket+`"}`))
	request.Header.Set("Origin", service.config.PublicURL.String())
	request.Header.Set("Content-Type", "application/json")
	request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: oldID})
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("consume failed %d", response.Code)
	}
	newID := ""
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == service.sessionCookieName() {
			newID = cookie.Value
		}
	}
	if newID == "" || newID == oldID {
		t.Fatal("no identity replacement")
	}
	t.Cleanup(func() {
		_ = store.client.Del(context.Background(), sessionKey(newID), sessionRootKey(newID), lineageMembersKey(newID), logoutMarkerKey(newID)).Err()
	})
	if _, err := store.GetSession(context.Background(), oldID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatal("old session remains")
	}
	if replay := consumeSessionHandoffTicket(service, ticket, "application/json", service.config.PublicURL.String()); replay.Code != http.StatusUnauthorized {
		t.Fatal("ticket replay succeeded")
	}
}

func TestF01ProxyExactScopeAndAnonymousRegistrationProof(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Cookie") != "" || r.Header.Get("X-Internal-Service-Token") != "" || r.Header.Get("X-Pinvou-Token-Account-ID") != "" {
			t.Error("browser-owned credential/header escaped allowlist")
		}
		if strings.HasPrefix(r.URL.Path, "/v1/auth/") {
			if r.Header.Get("Authorization") != "" {
				t.Error("anonymous route forwarded browser bearer")
			}
		} else if r.Header.Get("Authorization") != "Bearer test-access" {
			t.Error("protected route lost server session")
		}
		w.Header().Set("X-Context-Version", "3")
		w.Header().Set("X-Personal-Authorization-Version", "2")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	store := newMemoryStore()
	service := testService(t, store)
	service.config.AccountURL = parseTestURL(t, upstream.URL)
	service.config.TokenGatewayURL = service.config.AccountURL
	session := validSession(uuid.NewString())
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	id := "11111111-1111-4111-8111-111111111111"
	cases := []struct {
		method, path string
		want         int
		anonymous    bool
	}{
		{"POST", "/api/account/v1/auth/registration/precheck", 200, true}, {"POST", "/api/account/v1/auth/captcha/challenges", 200, true}, {"POST", "/api/account/v1/auth/sms/challenges", 200, true}, {"POST", "/api/account/v1/auth/register", 200, true}, {"POST", "/api/account/v1/auth/password-resets", 200, true}, {"GET", "/api/account/v1/auth/registration-operations/" + id, 200, true},
		{"POST", "/api/account/v1/account/bootstrap", 200, false}, {"GET", "/api/account/v1/me", 200, false}, {"HEAD", "/api/account/v1/me/authorization-context", 200, false}, {"POST", "/api/account/v1/me/password-changes", 200, false}, {"POST", "/api/account/v1/me/profile-change-requests", 200, false}, {"GET", "/api/account/v1/me/profile-change-requests/" + id, 200, false}, {"POST", "/api/account/v1/me/profile-change-requests/" + id + "/mobile-challenges", 200, false}, {"POST", "/api/account/v1/me/profile-change-requests/" + id + "/actions/complete", 200, false},
		{"GET", "/api/token/v1/me/balance", 200, false}, {"HEAD", "/api/token/v1/me/balance", 200, false}, {"GET", "/api/admin/account/admin/v1/users", 200, false}, {"PATCH", "/api/admin/account/admin/v1/users/" + id + "/status", 200, false},
		{"GET", "/api/admin/account/admin/v1/sms-configuration", 200, false}, {"HEAD", "/api/admin/account/admin/v1/sms-configuration", 200, false}, {"PUT", "/api/admin/account/admin/v1/sms-configuration", 200, false}, {"POST", "/api/admin/account/admin/v1/sms-configuration", 404, false}, {"DELETE", "/api/admin/account/admin/v1/sms-configuration", 404, false}, {"GET", "/api/admin/account/admin/v1/sms-configuration/history", 404, false},
		{"GET", "/api/admin/account/admin/v1/registration-operations", 200, false}, {"GET", "/api/admin/account/admin/v1/profile-change-requests", 200, false}, {"POST", "/api/admin/account/admin/v1/registration-operations/" + id + "/actions/retry", 200, false}, {"POST", "/api/admin/account/admin/v1/registration-operations/" + id + "/actions/compensate", 200, false}, {"POST", "/api/admin/account/admin/v1/profile-change-requests/" + id + "/actions/retry", 200, false}, {"POST", "/api/admin/account/admin/v1/profile-change-requests/" + id + "/actions/compensate", 404, false}, {"POST", "/api/admin/account/admin/v1/registration-operations/00000000-0000-0000-0000-000000000000/actions/retry", 404, false},
		{"GET", "/api/account/v1/organizations", 404, false}, {"POST", "/api/account/v1/me/closure-requests", 404, false}, {"POST", "/api/account/v1/approvals", 404, false}, {"GET", "/api/token/v1/models", 404, false}, {"POST", "/api/token/v1/chat/completions", 404, false}, {"GET", "/api/token/v1/recharge/products", 404, false}, {"POST", "/api/token/v1/recharge/orders", 404, false}, {"GET", "/api/token/v1/me/usage-requests", 404, false}, {"GET", "/api/admin/token/admin/v1/accounts", 404, false}, {"GET", "/api/admin/billing/admin/v1/payment-orders", 404, false}, {"DELETE", "/api/admin/account/admin/v1/users/" + id, 404, false}, {"PATCH", "/api/admin/account/admin/v1/users/00000000-0000-0000-0000-000000000000/status", 404, false}, {"GET", "/api/account/v1/me/../me", 404, false}, {"POST", "/api/account/v1/auth/register/", 404, true},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			before := calls.Load()
			request := httptest.NewRequest(c.method, c.path, strings.NewReader(`{}`))
			request.Header.Set("Origin", "https://portal.example")
			request.Header.Set("Authorization", "Bearer browser-owned")
			request.Header.Set("X-Internal-Service-Token", "browser-owned")
			request.Header.Set("X-Pinvou-Token-Account-ID", id)
			if !c.anonymous {
				request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
				request.AddCookie(&http.Cookie{Name: service.csrfCookieName(), Value: session.CSRFToken})
				request.Header.Set("X-CSRF-Token", session.CSRFToken)
			}
			response := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(response, request)
			if response.Code != c.want {
				t.Fatalf("status %d want %d", response.Code, c.want)
			}
			if c.want == 404 && calls.Load() != before {
				t.Fatal("denied path dispatched")
			}
			if c.want == 200 && (calls.Load() != before+1 || response.Header().Get("X-Context-Version") != "3") {
				t.Fatal("delivered route did not dispatch exact response")
			}
		})
	}
	for _, origin := range []string{"https://other.example", "", "https://portal.example/", "https://portal.example?", "https://portal.example/path"} {
		t.Run("anonymous origin "+origin, func(t *testing.T) {
			before := calls.Load()
			request := httptest.NewRequest("POST", "/api/account/v1/auth/register", strings.NewReader(`{}`))
			request.Header.Set("Origin", origin)
			response := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(response, request)
			if response.Code != 403 || calls.Load() != before {
				t.Fatal("anonymous registration crossed origin boundary")
			}
		})
	}
}

func TestProviderLifetimeAndCompleteJSONBoundaries(t *testing.T) {
	for _, seconds := range []int64{0, -1, int64((1<<63-1)/time.Second) + 1} {
		t.Run("invalid lifetime", func(t *testing.T) {
			tokens := TokenResponse{AccessToken: "a", RefreshToken: "r", TokenType: "Bearer", ExpiresIn: seconds, RefreshExpiresIn: 3600}
			if validTokenResponse(tokens, true) {
				t.Fatal("unrepresentable/invalid access lifetime accepted")
			}
			tokens.ExpiresIn = 60
			tokens.RefreshExpiresIn = seconds
			if validTokenResponse(tokens, true) {
				t.Fatal("invalid refresh lifetime accepted")
			}
		})
	}
	store := newMemoryStore()
	service, _ := oidcTestService(t, store, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(TokenResponse{AccessToken: "new-access", RefreshToken: "new-refresh", TokenType: "Bearer", ExpiresIn: 60, RefreshExpiresIn: 45 * 24 * 60 * 60})
	}))
	session := validSession(uuid.NewString())
	session.AccessExpiresAt = time.Now().Add(-time.Minute)
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	got, err := service.refreshSession(context.Background(), session)
	if err != nil || time.Until(got.RefreshExpiresAt) < 44*24*time.Hour {
		t.Fatal("arbitrary 30-day ceiling changed provider expiry")
	}
	for _, body := range []string{`{} {}`, strings.Repeat(" ", 1025) + `{}`} {
		t.Run("decode boundary", func(t *testing.T) {
			var dto struct {
				Value int `json:"value"`
			}
			err := decodePortalJSON(strings.NewReader(body), &dto, 1024)
			if err == nil {
				t.Fatal("incomplete/overflow JSON accepted")
			}
		})
	}
}
