package approvaleffect

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestClientReadsOnlyExactAuthoritativeObservation(t *testing.T) {
	requestID := uuid.Must(uuid.NewV7())
	fingerprint := strings.Repeat("a", 64)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet || r.URL.Path != "/internal/v1/approval-effects/"+requestID.String() ||
			r.URL.Query().Get("command_fingerprint") != fingerprint || r.Header.Get("Authorization") != "Bearer effect-reader" ||
			r.Header.Get("Cache-Control") != "no-cache" {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		commandType := r.URL.Query().Get("command_type")
		authority, _ := AuthorityForCommand(commandType)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(Observation{SchemaVersion: 1, Authority: authority, ApprovalRequestID: requestID,
			CommandType: commandType, CommandFingerprint: fingerprint, State: StateAbsent, ObservedAt: time.Now().UTC()})
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.URL, "effect-reader")
	if err != nil {
		t.Fatal(err)
	}
	for _, commandType := range []string{"WALLET_ADJUSTMENT", "TOKEN_CREDENTIAL_REVOKE"} {
		observation, err := client.Observe(context.Background(), requestID, commandType, fingerprint)
		if err != nil || observation.State != StateAbsent || observation.CommandType != commandType {
			t.Fatalf("%s observation=%+v err=%v", commandType, observation, err)
		}
	}
	if requests != 2 {
		t.Fatalf("requests=%d", requests)
	}
}

func TestClientFailsClosedOnTransportAndShapeDrift(t *testing.T) {
	requestID := uuid.Must(uuid.NewV7())
	fingerprint := strings.Repeat("b", 64)
	valid := Observation{SchemaVersion: 1, Authority: AuthorityBilling, ApprovalRequestID: requestID,
		CommandType: "WALLET_ADJUSTMENT", CommandFingerprint: fingerprint, State: StateAbsent, ObservedAt: time.Now().UTC()}
	encoded, _ := json.Marshal(valid)
	tests := map[string]func(http.ResponseWriter, *http.Request){
		"status": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		},
		"cache": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(encoded)
		},
		"content type": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(encoded)
		},
		"duplicate": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"schema_version":1,"schema_version":1}`))
		},
		"extra": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(append(encoded[:len(encoded)-1], []byte(`,"result":{}}`)...))
		},
		"binding": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "application/json")
			other := valid
			other.ApprovalRequestID = uuid.New()
			_ = json.NewEncoder(w).Encode(other)
		},
		"stale": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "application/json")
			other := valid
			other.ObservedAt = time.Now().UTC().Add(-time.Minute)
			_ = json.NewEncoder(w).Encode(other)
		},
	}
	for name, handler := range tests {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(handler))
			defer server.Close()
			client, err := NewClient(server.URL, server.URL, "effect-reader")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = client.Observe(context.Background(), requestID, "WALLET_ADJUSTMENT", fingerprint); err == nil {
				t.Fatal("unsafe observation was accepted")
			}
		})
	}
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/elsewhere", http.StatusFound) }))
	defer redirect.Close()
	client, _ := NewClient(redirect.URL, redirect.URL, "effect-reader")
	if _, err := client.Observe(context.Background(), requestID, "WALLET_ADJUSTMENT", fingerprint); err == nil {
		t.Fatal("redirect was followed")
	}
}

func TestClientRejectsInvalidConfigurationAndInput(t *testing.T) {
	for _, baseURL := range []string{"", "file:///tmp/service", "https://user@example.test", "https://example.test?x=1"} {
		if _, err := NewClient(baseURL, "https://token.example.test", "token"); err == nil {
			t.Fatalf("accepted billing URL %q", baseURL)
		}
	}
	if _, err := NewClient("https://billing.example.test", "https://token.example.test", " "); err == nil {
		t.Fatal("accepted empty credential")
	}
	client, err := NewClient("https://billing.example.test", "https://token.example.test", "token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Observe(context.Background(), uuid.New(), "UNKNOWN", strings.Repeat("a", 64)); err == nil {
		t.Fatal("accepted unknown command")
	}
}
