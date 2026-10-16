package token

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestMissingReferencePersonalFrozenPricingContract(t *testing.T) {
	for _, scenario := range []string{"personal-null", "organization-missing", "organization-nonnull", "price-null", "price-missing", "wallet-mismatch", "maximum-mismatch"} {
		t.Run(scenario, func(t *testing.T) {
			usage, _, snapshot := f05ExpiryFrozenFixture(t)
			hold := uuid.New()
			usage.HoldID = &hold
			switch scenario {
			case "organization-missing":
				delete(snapshot, "organization_id")
			case "organization-nonnull":
				snapshot["organization_id"] = uuid.New()
			case "price-null":
				snapshot["pricing_snapshot"] = nil
			case "price-missing":
				delete(snapshot, "pricing_snapshot")
			case "wallet-mismatch":
				snapshot["wallet_id"] = uuid.New()
			case "maximum-mismatch":
				usage.MaximumMicrocredits++
			}
			var err error
			usage.RequestSnapshot, err = json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			pricing, err := missingReferenceFrozenPricing(usage)
			if scenario == "personal-null" {
				if err != nil || pricing.ModelID != usage.Model {
					t.Fatal("complete frozen personal tuple rejected", err)
				}
			} else if !errors.Is(err, ErrForbidden) {
				t.Fatal("incomplete or foreign frozen facts accepted", err)
			}
		})
	}
}

// This is an actual loopback HTTP receipt contract test, not a database or
// financial release test. The real Billing/Token financial matrix is separate.
func TestMissingReferenceCheckedReleaseReceipt(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	for _, maximum := range []int64{0, 100} {
		for _, scenario := range []string{"complete", "missing-maximum", "null-maximum", "missing-wallet", "wrong-usage", "committed-status"} {
			t.Run(scenario+"-"+map[int64]string{0: "zero", 100: "positive"}[maximum], func(t *testing.T) {
				intent := missingReferenceRelease{Version: 1, UsageID: uuid.New(), UserID: uuid.New(), HoldID: uuid.New(), WalletID: uuid.New(), PriceID: uuid.New(), Maximum: maximum, Cause: missingReferenceDeadlineCause}
				body := map[string]any{"hold_id": intent.HoldID, "usage_request_id": intent.UsageID, "wallet_id": intent.WalletID, "price_version_id": intent.PriceID, "max_microcredits": maximum, "status": "RELEASED"}
				switch scenario {
				case "missing-maximum":
					delete(body, "max_microcredits")
				case "null-maximum":
					body["max_microcredits"] = nil
				case "missing-wallet":
					delete(body, "wallet_id")
				case "wrong-usage":
					body["usage_request_id"] = uuid.New()
				case "committed-status":
					body["status"] = "COMMITTED"
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodPost || r.URL.Path != "/internal/v1/holds/release" {
						t.Error("unexpected endpoint")
					}
					w.Header().Set("Content-Type", "application/json")
					if err := json.NewEncoder(w).Encode(body); err != nil {
						t.Error(err)
					}
				}))
				defer server.Close()
				client, err := NewHTTPBillingClient(server.URL, "controlled-deadline-receipt")
				if err != nil {
					t.Fatal(err)
				}
				receipt, err := client.ReleaseMissingReference(context.Background(), missingReferenceReleaseRequest{Intent: intent})
				if scenario == "complete" {
					if err != nil || receipt.Maximum != maximum || receipt.ID != intent.HoldID {
						t.Fatal("complete release receipt rejected", receipt, err)
					}
				} else if err == nil {
					t.Fatal("incomplete or foreign successful response accepted", receipt)
				}
			})
		}
	}
}
