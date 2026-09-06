package token

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type slowCredentialBody struct {
	io.Reader
	delay   time.Duration
	started bool
}

func (b *slowCredentialBody) Read(target []byte) (int, error) {
	if !b.started {
		b.started = true
		time.Sleep(b.delay)
	}
	return b.Reader.Read(target)
}
func (b *slowCredentialBody) Close() error { return nil }

func TestGenericCredentialClassificationKeepsEntryDeadline(t *testing.T) {
	original := tokenGovernanceHandlers["TOKEN_CREDENTIAL_VALIDATE"]
	defer func() { tokenGovernanceHandlers["TOKEN_CREDENTIAL_VALIDATE"] = original }()
	body, _ := json.Marshal(TokenCommandEnvelope{CommandType: "TOKEN_CREDENTIAL_VALIDATE"})
	for _, expired := range []bool{false, true} {
		deadline := time.Now().Add(120 * time.Millisecond)
		delay := 30 * time.Millisecond
		if expired {
			deadline = time.Now().Add(10 * time.Millisecond)
			delay = 25 * time.Millisecond
		}
		called := false
		handler := original
		handler.handle = func(_ *Service, w http.ResponseWriter, r *http.Request) {
			called = true
			actual, ok := r.Context().Deadline()
			if !ok || !actual.Equal(deadline) {
				t.Errorf("credential classifier reset deadline: %v expected %v", actual, deadline)
			}
			if time.Until(actual) > 100*time.Millisecond {
				t.Error("classification time was not charged")
			}
			// Downstream authorization/row waits inherit the same remaining budget.
			<-r.Context().Done()
			if time.Now().Before(deadline) {
				t.Error("deadline unexpectedly moved")
			}
			w.WriteHeader(503)
		}
		tokenGovernanceHandlers["TOKEN_CREDENTIAL_VALIDATE"] = handler
		request := httptest.NewRequest(http.MethodPost, "/admin/v1/commands", nil)
		request.Body = &slowCredentialBody{Reader: bytes.NewReader(body), delay: delay}
		request = request.WithContext(context.WithValue(request.Context(), credentialEntryDeadlineKey{}, deadline))
		response := httptest.NewRecorder()
		service := &Service{}
		TokenCredentialDeadlineMiddleware(http.HandlerFunc(service.adminCreateTokenCommand)).ServeHTTP(response, request)
		if expired && called {
			t.Fatal("expired classification reached credential business handler")
		}
		if !expired && !called {
			t.Fatalf("valid remaining budget rejected: %s", response.Body.String())
		}
		if response.Code != 503 {
			t.Fatalf("deadline response=%d", response.Code)
		}
	}
}
