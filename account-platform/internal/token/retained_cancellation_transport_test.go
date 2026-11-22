package token

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestRetainedReleaseAcknowledgement(t *testing.T) {
	usageID, holdID := uuid.New(), uuid.New()
	for _, test := range []struct {
		name, body string
		status     int
		valid      bool
	}{
		{"complete-success", `{"status":"RELEASED"}`, http.StatusOK, true},
		{"unavailable", `{"code":"BILLING_UNAVAILABLE"}`, http.StatusServiceUnavailable, false},
		{"extra-json", `{"status":"RELEASED"} {}`, http.StatusOK, false},
		{"overflow", `{"status":"RELEASED"}` + strings.Repeat(" ", 2<<20), http.StatusOK, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					UsageID uuid.UUID `json:"usage_request_id"`
					HoldID  uuid.UUID `json:"hold_id"`
					Reason  string    `json:"reason"`
				}
				if r.Method != http.MethodPost || r.URL.Path != "/internal/v1/holds/release" || json.NewDecoder(r.Body).Decode(&request) != nil || request.UsageID != usageID || request.HoldID != holdID || request.Reason != "cancelled before dispatch intent" {
					t.Error("release request facts changed")
				}
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			client, err := NewHTTPBillingClient(server.URL, "test-only")
			if err != nil {
				t.Fatal(err)
			}
			err = client.Release(context.Background(), usageID, holdID, "cancelled before dispatch intent")
			if (err == nil) != test.valid {
				t.Fatalf("release acknowledgement valid=%v err=%v", test.valid, err)
			}
		})
	}
}
