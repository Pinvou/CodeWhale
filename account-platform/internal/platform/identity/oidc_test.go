package identity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/gin-gonic/gin"
)

func TestMiddlewareRejectsMissingBearerToken(t *testing.T) {
	handler := Middleware(func(context.Context, string) (Principal, error) {
		t.Fatal("verifier must not be called")
		return Principal{}, nil
	}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("application handler must not be called")
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
}

func TestMiddlewareAddsVerifiedPrincipal(t *testing.T) {
	want := Principal{Issuer: "https://issuer.example", Subject: "subject", SessionID: "session"}
	handler := Middleware(func(_ context.Context, raw string) (Principal, error) {
		if raw != "token" {
			t.Fatalf("token = %q", raw)
		}
		return want, nil
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := FromContext(r.Context())
		if !ok || !reflect.DeepEqual(got, want) {
			t.Fatalf("principal = %+v, ok = %v", got, ok)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.Code)
	}
}

func TestRequireRealmRoleGinRejectsNonAdmin(t *testing.T) {
	verify := func(context.Context, string) (Principal, error) {
		return Principal{Issuer: "https://issuer.example", Subject: "subject", SessionID: "session", RealmRoles: []string{"member"}}, nil
	}
	router := httpserver.NewRouter()
	router.Use(GinMiddleware(verify), RequireRealmRoleGin("platform-admin"))
	router.GET("/admin", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	request := httptest.NewRequest(http.MethodGet, "/admin", nil)
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", response.Code)
	}
}

func TestRequireRealmRoleGinAllowsAdmin(t *testing.T) {
	verify := func(context.Context, string) (Principal, error) {
		return Principal{Issuer: "https://issuer.example", Subject: "subject", SessionID: "session", RealmRoles: []string{"platform-admin"}}, nil
	}
	router := httpserver.NewRouter()
	router.Use(GinMiddleware(verify), RequireRealmRoleGin("platform-admin"))
	router.GET("/admin", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	request := httptest.NewRequest(http.MethodGet, "/admin", nil)
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.Code)
	}
}

func TestOIDCVerifierRejectsHTTPInProduction(t *testing.T) {
	if _, err := NewOIDCVerifyFunc(context.Background(), "http://identity.example", "audience", "production", ""); err == nil {
		t.Fatal("expected insecure issuer to be rejected")
	}
}

func TestOIDCVerifierRejectsInsecureJWKSOverrideInProduction(t *testing.T) {
	if _, err := NewOIDCVerifyFunc(context.Background(), "https://identity.example", "audience", "production", "http://identity.internal/certs"); err == nil {
		t.Fatal("expected insecure JWKS URL to be rejected")
	}
}
