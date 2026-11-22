package portal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"sync"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
)

func internalAdmissionConfig(name string, burst int) config.Config {
	return config.Config{ServiceName: name, Environment: "test", RateLimitRPS: 0.001, RateLimitBurst: burst,
		HTTPTrustedProxyIPs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}
}

func portalInternalAdmissionHandler(s *Service) http.Handler {
	cfg := internalAdmissionConfig("portal-internal-fixture", 10)
	cfg.HTTPTrustedProxyIPs = []netip.Addr{netip.MustParseAddr("192.0.2.1")}
	return httpserver.HandlerWithApplication(cfg, s.Handler(nil), nil)
}

func internalAdmissionRequest(method, path, source string) *http.Request {
	r := httptest.NewRequest(method, path, http.NoBody)
	r.RemoteAddr = "192.0.2.1:40000"
	r.Header.Set(httpserver.RateLimitSourceHeader, source)
	r.Header.Set("X-Pinvou-Client-Source", "203.0.113.201")
	r.Header.Set("X-Forwarded-For", "203.0.113.202")
	return r
}

func TestPortalHandoffRateLimitSourceActualInternalCalls(t *testing.T) {
	const user = "11111111-1111-4111-8111-111111111111"
	s, _ := newSessionHandoffTestService(t, user)
	var mu sync.Mutex
	var accountSources []string
	account := httptest.NewServer(httpserver.HandlerWithApplication(internalAdmissionConfig("handoff-account-fixture", 2), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		accountSources = append(accountSources, r.Header.Get(httpserver.RateLimitSourceHeader))
		mu.Unlock()
		if r.Method != http.MethodGet || r.URL.Path != "/v1/me" || (r.Header.Get("Authorization") != "Bearer access" && r.Header.Get("Authorization") != "Bearer new-access") {
			t.Error("handoff Account request changed target or authentication")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user_id":"` + user + `"}`))
	}), nil))
	defer account.Close()
	providerCalls := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		providerCalls++
		mu.Unlock()
		if r.Header.Get(httpserver.RateLimitSourceHeader) != "" {
			t.Error("OIDC token exchange received the user admission hint")
		}
		if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:token-exchange" || r.Form.Get("subject_token") != "access" {
			t.Error("handoff no longer uses its original OIDC exchange")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-access","token_type":"Bearer","expires_in":900}`))
	}))
	defer provider.Close()
	s.config.AccountURL = parseTestURL(t, account.URL)
	s.config.TokenURL = parseTestURL(t, provider.URL+"/token")
	h := portalInternalAdmissionHandler(s)
	for i, source := range []string{"198.51.100.1", "198.51.100.2", "198.51.100.1"} {
		r := internalAdmissionRequest(http.MethodPost, "/portal-session-handoff", source)
		r.Header.Set("Authorization", "Bearer access")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		want := http.StatusOK
		if i == 2 {
			want = http.StatusServiceUnavailable // This source spent its two Account reads.
		}
		if w.Code != want {
			t.Fatalf("handoff request %d status=%d want=%d", i, w.Code, want)
		}
		if i < 2 {
			var result struct {
				Ticket string `json:"ticket"`
			}
			if json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Ticket == "" {
				t.Fatal("isolated handoff did not return its original ticket")
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(accountSources, []string{"198.51.100.1", "198.51.100.1", "198.51.100.2", "198.51.100.2"}) || providerCalls != 2 {
		t.Fatal("both handoff identity reads must preserve each admitted source; OIDC remains separate")
	}
}

func TestPortalSystemStatusRateLimitSourceActualInternalCalls(t *testing.T) {
	store := newMemoryStore()
	s := testService(t, store)
	session := validSession("controlled-status-admission")
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var accountSources, tokenSources []string
	account := httptest.NewServer(httpserver.HandlerWithApplication(internalAdmissionConfig("status-account-fixture", 1), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		accountSources = append(accountSources, r.Header.Get(httpserver.RateLimitSourceHeader))
		mu.Unlock()
		if r.Method != http.MethodPost || r.URL.Path != "/v1/account/bootstrap" || r.Header.Get("Authorization") != "Bearer "+session.AccessToken {
			t.Error("status Account bootstrap changed target or authentication")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"platform_permissions":["platform.system.status.read"]}`))
	}), nil))
	defer account.Close()
	token := httptest.NewServer(httpserver.HandlerWithApplication(internalAdmissionConfig("status-token-fixture", 2), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tokenSources = append(tokenSources, r.Header.Get(httpserver.RateLimitSourceHeader))
		mu.Unlock()
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+session.AccessToken {
			t.Error("status Token reads changed authentication")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/admin/v1/overview":
			_, _ = w.Write([]byte(`{"reconciliation_pending":3,"reconciliation_stopped":1}`))
		case "/admin/v1/usage-requests":
			if r.URL.Query().Get("limit") != "20" {
				t.Error("status usage query changed")
			}
			_, _ = w.Write([]byte(`{"items":[]}`))
		default:
			t.Error("unexpected status Token target")
			w.WriteHeader(http.StatusNotFound)
		}
	}), nil))
	defer token.Close()
	probeCalls := 0
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		probeCalls++
		mu.Unlock()
		if r.Header.Get(httpserver.RateLimitSourceHeader) != "" || r.Header.Get("Authorization") != "" {
			t.Error("configurable status probe received user source or authority")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer probe.Close()
	s.config.AccountURL = parseTestURL(t, account.URL)
	s.config.TokenGatewayURL = parseTestURL(t, token.URL)
	s.config.SystemProbes = []SystemProbe{{Key: "controlled-external", Target: parseTestURL(t, probe.URL)}}
	h := portalInternalAdmissionHandler(s)
	for i, source := range []string{"198.51.100.1", "198.51.100.2", "198.51.100.1"} {
		r := internalAdmissionRequest(http.MethodGet, "/api/admin/system/status", source)
		r.AddCookie(&http.Cookie{Name: s.sessionCookieName(), Value: session.ID})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		want := http.StatusOK
		if i == 2 {
			want = http.StatusServiceUnavailable // The first source's bootstrap budget is exhausted.
		}
		if w.Code != want {
			t.Fatalf("status request %d=%d want=%d", i, w.Code, want)
		}
		if i < 2 {
			var result systemStatusResponse
			if json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Reconciliation.Pending == nil || *result.Reconciliation.Pending != 3 || result.Reconciliation.Stopped == nil || *result.Reconciliation.Stopped != 1 || len(result.RecentErrors) != 0 {
				t.Fatal("status did not retain both authorized Token reconciliation reads")
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(accountSources, []string{"198.51.100.1", "198.51.100.2"}) || !reflect.DeepEqual(tokenSources, []string{"198.51.100.1", "198.51.100.1", "198.51.100.2", "198.51.100.2"}) || probeCalls != 2 {
		t.Fatal("status internal APIs must retain the admitted source; probes remain unmodified")
	}
}

func TestPortalInternalCallsWithoutUserContextDoNotInventSource(t *testing.T) {
	s := testService(t, newMemoryStore())
	calls := 0
	var mu sync.Mutex
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		if r.Header.Get(httpserver.RateLimitSourceHeader) != "" {
			t.Error("background context invented a user admission source")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/me":
			_, _ = w.Write([]byte(`{"user_id":"11111111-1111-4111-8111-111111111111"}`))
		case "/admin/v1/overview":
			_, _ = w.Write([]byte(`{"reconciliation_pending":0,"reconciliation_stopped":0}`))
		case "/admin/v1/usage-requests":
			_, _ = w.Write([]byte(`{"items":[]}`))
		default:
			t.Error("unexpected fixed internal API")
		}
	}))
	defer backend.Close()
	s.config.AccountURL = parseTestURL(t, backend.URL)
	s.config.TokenGatewayURL = parseTestURL(t, backend.URL)
	if _, err := s.accountUserID(context.Background(), "controlled-access"); err != nil {
		t.Fatal(err)
	}
	result := s.collectSystemStatus(context.Background(), "controlled-access")
	mu.Lock()
	defer mu.Unlock()
	if calls != 3 || len(result.RecentErrors) != 0 {
		t.Fatal("background fixed API calls changed their original behavior")
	}
}
