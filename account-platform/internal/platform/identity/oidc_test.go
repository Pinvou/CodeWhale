package identity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
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
		if !ok || got != want {
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

func TestOIDCVerifierRejectsHTTPInProduction(t *testing.T) {
	if _, err := NewOIDCVerifyFunc(context.Background(), "http://identity.example", "audience", "production", ""); err == nil {
		t.Fatal("expected insecure issuer to be rejected")
	}
}
