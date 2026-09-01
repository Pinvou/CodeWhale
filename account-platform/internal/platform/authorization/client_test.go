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

func TestClientRequestsBoundHumanDelegation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/authorization/human-delegations" || r.Header.Get("Authorization") != "Bearer service-secret" {
			t.Fatalf("unexpected delegation request: %s %s", r.URL.Path, r.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["issuer"] != "issuer" || body["subject"] != "subject" || body["request_sha256"] != RequestSHA256([]byte("request")) {
			t.Fatalf("delegation binding omitted: %#v", body)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"delegation":"signed-delegation"}`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "service-secret")
	if err != nil {
		t.Fatal(err)
	}
	token, err := client.Delegate(context.Background(), identity.Principal{Issuer: "issuer", Subject: "subject"}, DelegationRequest{Audience: "billing-service", Permission: "org.quota.allocate", TargetResourceType: "member-quota", TargetResourceID: "grant-1", HTTPMethod: http.MethodPost, HTTPPath: "/internal/v1/member-quota-grants", RequestSHA256: RequestSHA256([]byte("request"))})
	if err != nil || token != "signed-delegation" {
		t.Fatalf("unexpected result: %q %v", token, err)
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
			Permission string `json:"permission"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("X-Context-Version", "3")
		w.Header().Set("X-Authorization-Version", "5")
		_ = json.NewEncoder(w).Encode(map[string]bool{"allowed": body.Permission == "platform.users.read"})
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
	if response.Code != http.StatusNoContent || checks != 2 || response.Header().Get("X-Authorization-Version") != "5" {
		t.Fatalf("status=%d checks=%d headers=%v", response.Code, checks, response.Header())
	}
}
