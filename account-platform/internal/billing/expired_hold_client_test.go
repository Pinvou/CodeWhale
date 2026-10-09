package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestExpiredHoldHTTPProofRequiresBoundConfirmedReceipt(t *testing.T) {
	identity := ExpiredHoldIdentity{UsageRequestID: uuid.New(), HoldID: uuid.New(), WalletID: uuid.New(), MaxMicrocredits: 9007199254740993, ExpiresAt: time.Now().UTC().Add(-time.Hour)}
	for _, scenario := range []string{"confirmed", "wrong-wallet", "only-fenced", "redirect", "provider-error", "oversized", "trailing-json"} {
		t.Run(scenario, func(t *testing.T) {
			var redirected atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/redirected" {
					redirected.Add(1)
					return
				}
				if r.Method != http.MethodPost || r.URL.Path != "/internal/v1/holds/expiry-proof" || r.Header.Get("Authorization") != "Bearer fixture-workload-token" {
					t.Error("incorrect authenticated recovery request")
				}
				var received ExpiredHoldIdentity
				if json.NewDecoder(r.Body).Decode(&received) != nil || !received.Matches(identity) {
					t.Error("reservation identity or exact int64 amount lost on wire")
				}
				proof := ExpiredHoldProof{Identity: identity, ProofID: uuid.New(), Status: "LOCAL_CONFIRMED"}
				switch scenario {
				case "wrong-wallet":
					proof.WalletID = uuid.New()
				case "only-fenced":
					proof.Status = "PRE_DISPATCH_FENCED"
				case "redirect":
					http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
					return
				case "provider-error":
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				case "oversized":
					w.Write([]byte(strings.Repeat(" ", 65537)))
					return
				}
				json.NewEncoder(w).Encode(proof)
				if scenario == "trailing-json" {
					w.Write([]byte(`{}`))
				}
			}))
			defer server.Close()
			client, err := NewHTTPExpiredHoldProofClient(server.URL, "fixture-workload-token", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.FenceExpiredHold(context.Background(), identity)
			if (err == nil) != (scenario == "confirmed") || redirected.Load() != 0 {
				t.Fatal("unproven result accepted or credentials followed redirect")
			}
		})
	}
}
