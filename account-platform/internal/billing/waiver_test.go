package billing

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPWaiverCoordinatorRejectsMalformedAuthorityAndPreservesConflict(t *testing.T) {
	id, usage, wallet := uuid.New(), uuid.New(), uuid.New()
	zero := int64(0)
	now := time.Now().UTC()
	for _, scenario := range []string{"valid", "valid-zero", "wrong-usage", "missing-usage", "missing-wallet", "missing-maximum", "missing-time", "nonzero-charge", "missing-waiver-proof", "malformed", "redirect", "committed-conflict"} {
		t.Run(scenario, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/internal/v1/usage-waivers" || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("c", 32) {
					t.Error("coordinator changed its private authenticated endpoint")
					w.WriteHeader(400)
					return
				}
				var intent map[string]any
				if json.NewDecoder(r.Body).Decode(&intent) != nil || intent["hold_id"] != id.String() || intent["usage_request_id"] != usage.String() {
					t.Error("coordinator did not bind the requested financial identity")
					w.WriteHeader(400)
					return
				}
				result := WaiverAuthority{Hold: AdminHold{ID: id, UsageRequestID: usage, WalletID: wallet, Status: "RELEASED", MaxMicrocredits: 100, ActualMicrocredits: &zero, ExpiresAt: now.Add(time.Hour), UpdatedAt: now}, UsageSnapshot: json.RawMessage(`{"admin_waiver":true,"admin_actor":"fixture","admin_release_reason":"fixture"}`)}
				switch scenario {
				case "wrong-usage":
					result.Hold.UsageRequestID = uuid.New()
				case "missing-usage":
					result.Hold.UsageRequestID = uuid.Nil
				case "missing-wallet":
					result.Hold.WalletID = uuid.Nil
				case "valid-zero":
					result.Hold.MaxMicrocredits = 0
				case "missing-maximum":
					encoded, _ := json.Marshal(result)
					var missing map[string]json.RawMessage
					_ = json.Unmarshal(encoded, &missing)
					var hold map[string]json.RawMessage
					_ = json.Unmarshal(missing["hold"], &hold)
					delete(hold, "max_microcredits")
					missing["hold"], _ = json.Marshal(hold)
					json.NewEncoder(w).Encode(missing)
					return
				case "missing-time":
					result.Hold.UpdatedAt = time.Time{}
				case "nonzero-charge":
					positive := int64(5)
					result.Hold.ActualMicrocredits = &positive
				case "missing-waiver-proof":
					result.UsageSnapshot = json.RawMessage(`{}`)
				case "malformed":
					w.Write([]byte(`{"hold":`))
					return
				case "redirect":
					http.Redirect(w, r, "/unexpected", 302)
					return
				case "committed-conflict":
					w.WriteHeader(409)
					return
				}
				json.NewEncoder(w).Encode(result)
			}))
			defer server.Close()
			client, err := NewHTTPWaiverCoordinator(server.URL, strings.Repeat("c", 32))
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Release(context.Background(), id, usage, "fixture", "fixture")
			if scenario == "valid" || scenario == "valid-zero" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil {
				t.Fatal("unproved or unbound response was reported as successful waiver")
			}
			if errors.Is(err, ErrUsageAlreadyTerminal) != (scenario == "committed-conflict") {
				t.Fatal("malformed transport result was mistaken for an authoritative financial conflict")
			}
		})
	}
}
