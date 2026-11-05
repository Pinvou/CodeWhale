package billing

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestProviderBlindRequiresStableCommandKey(t *testing.T) {
	service := NewService(&PostgresRepository{})
	for _, fixture := range []struct{ name, key string }{{"missing", ""}, {"short", "short"}, {"oversized", strings.Repeat("x", 129)}} {
		t.Run(fixture.name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/controlled-handler", strings.NewReader(`{"reason":"controlled unavailable provider history","command_protocol":1}`))
			request.SetPathValue("case_id", uuid.NewString())
			request.Header.Set("Idempotency-Key", fixture.key)
			response := httptest.NewRecorder()
			service.adminEscalateProviderBlind(response, request)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "IDEMPOTENCY_KEY_REQUIRED") {
				t.Fatal("missing/invalid identity reached repository", response.Code, response.Body.String())
			}
		})
	}
}

func TestProviderBlindRequiresExplicitCommandProtocol(t *testing.T) {
	service := NewService(&PostgresRepository{})
	for _, suffix := range []string{"", `,"command_protocol":0`, `,"command_protocol":2`} {
		request := httptest.NewRequest("POST", "/controlled-handler", strings.NewReader(`{"reason":"controlled unavailable provider history"`+suffix+`}`))
		request.SetPathValue("case_id", uuid.NewString())
		request.Header.Set("Idempotency-Key", "controlled-original-unknown-key")
		response := httptest.NewRecorder()
		service.adminEscalateProviderBlind(response, request)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "COMMAND_PROTOCOL_REQUIRED") {
			t.Fatal("unversioned intent reached repository", response.Code, response.Body.String())
		}
	}
}
