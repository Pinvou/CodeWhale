package serviceauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestMiddlewareRequiresExactToken(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for _, test := range []struct {
		header string
		want   int
	}{
		{"", http.StatusUnauthorized},
		{"Bearer wrong", http.StatusUnauthorized},
		{"Bearer correct-token", http.StatusNoContent},
	} {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.Header.Set("Authorization", test.header)
		response := httptest.NewRecorder()
		Middleware("correct-token", next).ServeHTTP(response, request)
		if response.Code != test.want {
			t.Fatalf("header %q: status = %d, want %d", test.header, response.Code, test.want)
		}
	}
}

func TestProjectedCredentialIsReloadedForEveryRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	if err := SetAuthorization(request, "file:"+path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetAuthorization(request, "file:"+path); err != nil {
		t.Fatal(err)
	}
	if got := request.Header.Get("Authorization"); got != "Bearer second" {
		t.Fatalf("rotated token was not reloaded: %q", got)
	}
}

func TestWorkloadRulesDefaultDenyAndExposeServiceIdentity(t *testing.T) {
	auth := authenticator{verify: func(context.Context, string) (string, error) {
		return "system:serviceaccount:pinvou:billing-service", nil
	}, rules: []rule{{Subject: "system:serviceaccount:pinvou:billing-service", Service: "billing-service", Methods: []string{http.MethodPost}, Paths: []string{"/internal/v1/authorization/*"}}}}
	allowed := httptest.NewRequest(http.MethodPost, "/internal/v1/authorization/check", nil)
	allowed.Header.Set("Authorization", "Bearer signed")
	identity, status := auth.authenticate(allowed)
	if status != http.StatusOK || identity.ActorType != "SERVICE" || identity.Service != "billing-service" {
		t.Fatalf("unexpected decision: %#v %d", identity, status)
	}
	denied := httptest.NewRequest(http.MethodDelete, "/internal/v1/authorization/check", nil)
	denied.Header.Set("Authorization", "Bearer signed")
	if _, status := auth.authenticate(denied); status != http.StatusForbidden {
		t.Fatalf("expected forbidden, got %d", status)
	}
}

func TestStaticServiceTokenRejectedOutsideDevelopment(t *testing.T) {
	t.Setenv("APP_ENV", "staging")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	request := httptest.NewRequest(http.MethodGet, "/internal/v1/check", nil)
	request.Header.Set("Authorization", "Bearer legacy")
	if _, status := newAuthenticator("legacy").authenticate(request); status != http.StatusUnauthorized {
		t.Fatalf("expected fail closed, got %d", status)
	}
}

func TestBackgroundIdentityCannotMasqueradeAsHuman(t *testing.T) {
	ctx, err := ContextForJob(context.Background(), "account-event-relay", "OUTBOX_RELAY", "run-1")
	if err != nil {
		t.Fatal(err)
	}
	identity, ok := FromContext(ctx)
	if !ok || identity.ActorType != "SERVICE" || identity.JobRunID != "run-1" {
		t.Fatalf("unexpected background identity: %#v", identity)
	}
}
