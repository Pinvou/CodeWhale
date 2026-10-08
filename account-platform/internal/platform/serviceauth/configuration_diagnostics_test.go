package serviceauth

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInvalidRulesAreDiagnosedWithoutLeakingConfiguration(t *testing.T) {
	const marker = "private-configuration-marker"
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "https://issuer.example")
	t.Setenv("WORKLOAD_IDENTITY_JWKS_URL", "https://issuer.example/certs")
	t.Setenv("WORKLOAD_IDENTITY_AUDIENCE", "service")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", marker)
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	called := false
	handler := Middleware("private-token", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	request := httptest.NewRequest(http.MethodGet, "/internal", nil)
	request.Header.Set("Authorization", "Bearer private-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || called {
		t.Fatal("invalid configuration did not fail closed")
	}
	if !strings.Contains(output.String(), "WORKLOAD_RULES_INVALID") || strings.Contains(output.String(), marker) || strings.Contains(output.String(), "private-token") {
		t.Fatalf("unsafe or absent startup diagnostic: %s", output.String())
	}
}
