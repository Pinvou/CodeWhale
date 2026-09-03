package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestApprovalExecutionClientDoesNotSendExecutorIdentity(t *testing.T) {
	requestID := uuid.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer service-token" {
			t.Fatalf("missing service authentication")
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Fatalf("invalid request body")
		}
		if _, supplied := body["executor_identity"]; supplied {
			t.Fatal("executor identity must be derived by Account from service authentication")
		}
		if strings.HasSuffix(r.URL.Path, "/complete") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_ = json.NewEncoder(w).Encode(ApprovedCommand{ApprovalRequestID: requestID, ExecutionIdempotencyKey: "approval:key"})
	}))
	t.Cleanup(server.Close)
	gate, err := NewHTTPApprovalExecutionGate(server.URL, "service-token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gate.Begin(context.Background(), requestID, "job-1", json.RawMessage(`{"version":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := gate.Complete(context.Background(), requestID, "job-1", "EXECUTED", json.RawMessage(`{}`), json.RawMessage(`{}`), ""); err != nil {
		t.Fatal(err)
	}
}
