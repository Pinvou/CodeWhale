package portal

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSystemReconciliationUnavailableIsNotZero(t *testing.T) {
	for _, tc := range []struct {
		name, overview, usage string
		status                int
		wantCounts            bool
		errors                int
	}{
		{"real_zero", `{"reconciliation_pending":0,"reconciliation_stopped":0}`, `{"items":[]}`, 200, true, 0},
		{"forbidden", `{}`, `{}`, 403, false, 2},
		{"upstream_failure", `{}`, `{}`, 503, false, 2},
		{"invalid_json", `{`, `{`, 200, false, 2},
		{"missing_counts", `{}`, `{"items":[]}`, 200, false, 1},
		{"negative_counts", `{"reconciliation_pending":-1,"reconciliation_stopped":0}`, `{"items":[]}`, 200, false, 1},
		{"missing_error_list", `{"reconciliation_pending":0,"reconciliation_stopped":0}`, `{}`, 200, true, 1},
		{"null_error_list", `{"reconciliation_pending":0,"reconciliation_stopped":0}`, `{"items":null}`, 200, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer controlled-token" || r.Header.Get("Cookie") != "" {
					t.Error("status read authority changed")
				}
				w.WriteHeader(tc.status)
				if r.URL.Path == "/admin/v1/overview" {
					io.WriteString(w, tc.overview)
				} else if r.URL.Path == "/admin/v1/usage-requests" {
					io.WriteString(w, tc.usage)
				} else {
					t.Error("unexpected endpoint")
				}
			}))
			defer backend.Close()
			service := testService(t, newMemoryStore())
			service.config.TokenGatewayURL = parseTestURL(t, backend.URL)
			result := service.reconciliationStatus(context.Background(), "controlled-token")
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]any
			if err = json.Unmarshal(encoded, &wire); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"pending", "stopped"} {
				if tc.wantCounts {
					if wire[key] != float64(0) {
						t.Fatalf("real zero %s=%v", key, wire[key])
					}
				} else if wire[key] != nil {
					t.Fatalf("unavailable %s masquerades as %v", key, wire[key])
				}
			}
			if len(result.RecentErrors) != tc.errors {
				t.Fatalf("diagnostics=%d want=%d", len(result.RecentErrors), tc.errors)
			}
		})
	}
	t.Run("network_failure", func(t *testing.T) {
		backend := httptest.NewServer(http.NotFoundHandler())
		target := backend.URL
		backend.Close()
		service := testService(t, newMemoryStore())
		service.config.TokenGatewayURL = parseTestURL(t, target)
		result := service.reconciliationStatus(context.Background(), "controlled-token")
		b, _ := json.Marshal(result)
		var wire map[string]any
		json.Unmarshal(b, &wire)
		if wire["pending"] != nil || wire["stopped"] != nil || len(result.RecentErrors) != 2 {
			t.Fatalf("network error lost: %s", b)
		}
	})
}
