package portal

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestF18RelayMetricNumbersThroughHTTP(t *testing.T) {
	for _, test := range []struct{ name, pending, dead, age, want string }{
		{"valid-empty", "0", "0", "0", "HEALTHY"},
		{"valid-pending", "2", "0", "3.5", "HEALTHY"},
		{"valid-dead-letter", "2", "1", "3.5", "DEGRADED"},
		{"nan-pending", "NaN", "0", "0", "DEGRADED"},
		{"infinite-dead-letter", "0", "+Inf", "0", "DEGRADED"},
		{"negative-pending", "-1", "0", "0", "DEGRADED"},
		{"fractional-dead-letter", "0", "0.5", "0", "DEGRADED"},
		{"overflow-pending", "9223372036854775808", "0", "0", "DEGRADED"},
		{"nan-age", "0", "0", "NaN", "DEGRADED"},
		{"negative-age", "0", "0", "-0.5", "DEGRADED"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
					t.Error("relay probe forwarded authority")
				}
				switch r.URL.Path {
				case "/health/live":
					w.WriteHeader(204)
				case "/metrics":
					fmt.Fprintf(w, "pinvou_outbox_pending_events %s\npinvou_outbox_dead_lettered_events %s\npinvou_outbox_oldest_pending_age_seconds %s\n", test.pending, test.dead, test.age)
				default:
					t.Error("wrong probe path")
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			service := testService(t, newMemoryStore())
			got := service.probeRelay(context.Background(), "controlled-relay", parseTestURL(t, server.URL))
			if got.Status != test.want {
				t.Fatalf("actual relay status=%s want=%s counts=%d/%d age=%v", got.Status, test.want, got.PendingEvents, got.DeadLetteredEvents, got.OldestPendingSecond)
			}
			if _, err := json.Marshal(got); err != nil {
				t.Fatalf("actual status cannot serialize: %v", err)
			}
			if test.name == "valid-pending" && (got.PendingEvents != 2 || got.DeadLetteredEvents != 0 || got.OldestPendingSecond != 3.5) {
				t.Fatal("valid metric facts changed")
			}
		})
	}
}
