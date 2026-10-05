package portal

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type statusFailureStore struct{ *memoryStore }

func (s statusFailureStore) GetSession(context.Context, string) (Session, error) {
	return Session{}, errors.New("controlled store unavailable")
}

func TestSystemStatusPreservesTransientSessionFailures(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		store  Store
		cookie bool
		status int
		code   string
	}{
		{"store-unavailable", statusFailureStore{newMemoryStore()}, true, 503, "SESSION_UNAVAILABLE"},
		{"missing-session", newMemoryStore(), true, 401, "LOGIN_REQUIRED"},
		{"missing-cookie", newMemoryStore(), false, 401, "LOGIN_REQUIRED"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			service := testService(t, scenario.store)
			request := httptest.NewRequest("GET", "/api/admin/system/status", nil)
			if scenario.cookie {
				request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: "fixture-session"})
			}
			response := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(response, request)
			var body struct {
				Code string `json:"code"`
			}
			if response.Code != scenario.status || json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Code != scenario.code {
				t.Fatal("operational status confused a transient session failure with logout")
			}
		})
	}
}

func TestReconciliationStatusDisclosesUnreadableCountersAndErrors(t *testing.T) {
	for _, scenario := range []struct {
		name, body string
		status     int
	}{
		{"server-failure", "fixture-sensitive-detail", 500},
		{"forbidden", "fixture-sensitive-detail", 403},
		{"malformed", "fixture-sensitive-detail", 200},
		{"missing-shape", `{}`, 200},
		{"trailing-json", `{}{"private":"fixture-sensitive-detail"}`, 200},
		{"oversized", `{}` + strings.Repeat(" ", 1<<20), 200},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer fixture-admin" {
					t.Error("admin status omitted its authorization")
				}
				w.WriteHeader(scenario.status)
				_, _ = w.Write([]byte(scenario.body))
			}))
			defer server.Close()
			service := testService(t, newMemoryStore())
			target, _ := url.Parse(server.URL)
			service.config.TokenGatewayURL = target
			result := service.reconciliationStatus(context.Background(), "fixture-admin")
			if len(result.RecentErrors) != 2 || result.Pending != 0 || result.Stopped != 0 {
				t.Fatal("unknown operational state was reported as error-free counters")
			}
			body, _ := json.Marshal(result)
			if strings.Contains(string(body), "fixture-sensitive-detail") || strings.Contains(string(body), server.URL) || strings.Contains(string(body), "fixture-admin") {
				t.Fatal("status failure disclosed private transport data")
			}
		})
	}
}
