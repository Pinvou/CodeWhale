package portal

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestCredentialHandoffIsFixedMinimalAndHashPinned(t *testing.T) {
	service := testService(t, newMemoryStore())
	service.config.AdminTokenDetailsEnabled = true
	service.config.CredentialOrigin, _ = url.Parse("https://credentials-uat.account.pinvou.com")
	response := httptest.NewRecorder()
	service.Handler(http.NotFoundHandler()).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/credential-handoff", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	digest := sha256.Sum256([]byte(credentialHandoffScript))
	expectedHash := base64.StdEncoding.EncodeToString(digest[:])
	if expectedHash != credentialHandoffScriptSHA256 {
		t.Fatalf("generated handoff digest is stale: got %q want %q", credentialHandoffScriptSHA256, expectedHash)
	}
	csp := response.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'sha256-"+expectedHash+"'") || strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "'unsafe-inline'") || strings.Contains(csp, "'unsafe-eval'") {
		t.Fatalf("handoff CSP is not hash-pinned: %q", csp)
	}
	for key, expected := range map[string]string{
		"Cache-Control":                "no-store",
		"Referrer-Policy":              "origin",
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Cross-Origin-Resource-Policy": "same-origin",
	} {
		if got := response.Header().Get(key); got != expected {
			t.Fatalf("%s=%q want %q", key, got, expected)
		}
	}
	if response.Header().Get("Set-Cookie") != "" {
		t.Fatal("handoff must not create a cookie or session")
	}
	body := response.Body.String()
	for _, required := range []string{"credential-handoff", "HANDOFF_READY", "HANDOFF_BOUND", "HANDOFF_SUBMIT", "HANDOFF_ACCEPTED", "navigator.serviceWorker", "credentials-uat.account.pinvou.com"} {
		if !strings.Contains(body, required) {
			t.Fatalf("handoff document misses %q", required)
		}
	}
	for _, forbidden := range []string{"/assets/admin.js", "/assets/admin.css", "localStorage", "sessionStorage", "indexedDB", "document.cookie"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("handoff document contains forbidden capability %q", forbidden)
		}
	}
}

func TestCredentialHandoffRejectsStateInURLAndDisabledGate(t *testing.T) {
	service := testService(t, newMemoryStore())
	service.config.AdminTokenDetailsEnabled = true
	service.config.CredentialOrigin, _ = url.Parse("https://credentials-uat.account.pinvou.com")
	for _, target := range []string{"/credential-handoff?launch_id=secret", "/credential-handoff/extra"} {
		response := httptest.NewRecorder()
		service.Handler(http.NotFoundHandler()).ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s status=%d", target, response.Code)
		}
	}
	service.config.AdminTokenDetailsEnabled = false
	response := httptest.NewRecorder()
	service.Handler(http.NotFoundHandler()).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/credential-handoff", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("disabled status=%d", response.Code)
	}
}
