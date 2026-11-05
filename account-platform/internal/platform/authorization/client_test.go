package authorization

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/gin-gonic/gin"
)

func TestClientUsesServiceAuthenticationAndReturnsVersions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/authorization/check" || r.Header.Get("Authorization") != "Bearer service-secret" {
			t.Fatalf("unexpected authorization request: %s %s", r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Header().Set("X-Context-Version", "7")
		w.Header().Set("X-Authorization-Version", "11")
		_, _ = w.Write([]byte(`{"allowed":true}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "service-secret")
	if err != nil {
		t.Fatal(err)
	}
	allowed, headers, err := client.Allowed(context.Background(), identity.Principal{Issuer: "issuer", Subject: "subject"}, "platform.users.read")
	if err != nil || !allowed || headers.Get("X-Context-Version") != "7" || headers.Get("X-Authorization-Version") != "11" {
		t.Fatalf("allowed=%v headers=%v error=%v", allowed, headers, err)
	}
}

func TestClientRejectsInvalidConfiguration(t *testing.T) {
	if _, err := NewClient("not-a-url", "secret"); err == nil {
		t.Fatal("relative Account URL must be rejected")
	}
	if _, err := NewClient("https://account.example", ""); err == nil {
		t.Fatal("empty service token must be rejected")
	}
}

func TestRequireAnyRecordsOnlyVerifiedPermissions(t *testing.T) {
	checks := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checks++
		var body struct {
			AnyPermissions []string `json:"any_permissions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("X-Context-Version", "3")
		w.Header().Set("X-Authorization-Version", "5")
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": true, "granted_permissions": []string{"platform.users.read"}})
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "service-secret")
	if err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(identity.GinMiddleware(func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "issuer", Subject: "subject"}, nil
	}))
	router.GET("/overview", client.RequireAny("platform.users.read", "platform.organizations.read"), func(c *gin.Context) {
		if !GrantedPermission(c.Request.Context(), "platform.users.read") || GrantedPermission(c.Request.Context(), "platform.organizations.read") {
			t.Fatal("request context did not preserve the exact granted permission set")
		}
		c.Status(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodGet, "/overview", nil)
	request.Header.Set("Authorization", "Bearer user-token")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || checks != 1 || response.Header().Get("X-Authorization-Version") != "5" {
		t.Fatalf("status=%d checks=%d headers=%v", response.Code, checks, response.Header())
	}
}

func TestRequireAnyAndUsesOneAtomicVersionAndFailsClosedWithoutVersion(t *testing.T) {
	for _, test := range []struct {
		name       string
		version    string
		wantStatus int
	}{
		{name: "one atomic decision", version: "9", wantStatus: http.StatusNoContent},
		{name: "missing version", wantStatus: http.StatusServiceUnavailable},
		{name: "invalid version", version: "not-a-version", wantStatus: http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			checks := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				checks++
				var body struct {
					Any []string `json:"any_permissions"`
					All []string `json:"all_permissions"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if len(body.Any) != 2 || len(body.All) != 1 || body.All[0] != "platform.audit.read" {
					t.Fatalf("unexpected composite expression: %#v", body)
				}
				if test.version != "" {
					w.Header().Set("X-Authorization-Version", test.version)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"allowed": true, "granted_permissions": []string{"platform.models.read", "platform.audit.read"}})
			}))
			defer server.Close()
			client, err := NewClient(server.URL, "service-secret")
			if err != nil {
				t.Fatal(err)
			}
			gin.SetMode(gin.TestMode)
			router := gin.New()
			router.Use(identity.GinMiddleware(func(context.Context, string) (identity.Principal, error) {
				return identity.Principal{Issuer: "issuer", Subject: "subject"}, nil
			}))
			called := false
			router.GET("/audit", client.RequireAnyAnd([]string{"platform.models.read", "platform.tokens.read"}, []string{"platform.audit.read"}), func(c *gin.Context) {
				called = true
				if !GrantedPermission(c.Request.Context(), "platform.models.read") || !GrantedPermission(c.Request.Context(), "platform.audit.read") {
					t.Fatal("atomic grant set was not propagated")
				}
				c.Status(http.StatusNoContent)
			})
			request := httptest.NewRequest(http.MethodGet, "/audit", nil)
			request.Header.Set("Authorization", "Bearer user-token")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.wantStatus || checks != 1 || called != (test.wantStatus == http.StatusNoContent) {
				t.Fatalf("status=%d checks=%d called=%v", response.Code, checks, called)
			}
		})
	}
}
