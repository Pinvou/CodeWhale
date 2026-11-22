package token

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCapturedReferenceReleaseReceipt(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	price := uuid.New()
	intent := capturedReferenceRelease{Version: 1, UsageID: uuid.New(), HoldID: uuid.New(), WalletID: uuid.New(), PriceID: &price, Maximum: 0}
	good := confirmedReleaseReceipt{ID: intent.HoldID, UsageID: intent.UsageID, WalletID: intent.WalletID, PriceID: intent.PriceID, Maximum: 0, Status: "RELEASED"}
	for _, name := range []string{"exact", "nullable-price", "missing-price", "missing-zero-maximum", "wrong-price", "wrong-usage", "wrong-hold", "wrong-wallet", "wrong-maximum", "wrong-status", "malformed", "trailing"} {
		t.Run(name, func(t *testing.T) {
			v, receipt := intent, good
			if name == "nullable-price" {
				v.PriceID = nil
				receipt.PriceID = nil
			}
			if name == "wrong-price" {
				id := uuid.New()
				receipt.PriceID = &id
			}
			if name == "wrong-usage" {
				receipt.UsageID = uuid.New()
			}
			if name == "wrong-hold" {
				receipt.ID = uuid.New()
			}
			if name == "wrong-wallet" {
				receipt.WalletID = uuid.New()
			}
			if name == "wrong-maximum" {
				receipt.Maximum = -1
			}
			if name == "wrong-status" {
				receipt.Status = "COMMITTED"
			}
			raw, _ := json.Marshal(receipt)
			if name == "missing-price" || name == "missing-zero-maximum" {
				var fields map[string]json.RawMessage
				_ = json.Unmarshal(raw, &fields)
				if name == "missing-price" {
					delete(fields, "price_version_id")
				} else {
					delete(fields, "max_microcredits")
				}
				raw, _ = json.Marshal(fields)
			}
			if name == "malformed" {
				raw = []byte(`{"status":`)
			}
			if name == "trailing" {
				raw = append(raw, []byte(` {}`)...)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]json.RawMessage
				if r.Method != "POST" || r.URL.Path != "/internal/v1/holds/release" || json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 3 || body["expected_original_hold"] != nil {
					t.Error("generic original release contract changed")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(raw)
			}))
			defer server.Close()
			client, err := NewHTTPBillingClient(server.URL, "controlled")
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.ReleaseCapturedReference(context.Background(), v)
			want := name == "exact" || name == "nullable-price"
			if (err == nil) != want {
				t.Fatalf("ack=%v expected success=%v", err, want)
			}
		})
	}
}

func TestCapturedReferenceTerminalExtensionProof(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	for _, name := range []string{"exact", "nullable-omitted", "release-201", "release-503", "release-wrong-hold", "release-smaller", "proof-201", "proof-503", "wrong-fingerprint", "wrong-lease", "proof-wrong-wallet", "proof-wrong-maximum", "proof-actual", "proof-missing-maximum", "proof-null-maximum", "proof-missing-expiry", "proof-wrong-price", "proof-malformed"} {
		t.Run(name, func(t *testing.T) {
			p := uuid.New()
			v := capturedReferenceRelease{UsageID: uuid.New(), HoldID: uuid.New(), WalletID: uuid.New(), PriceID: &p, Maximum: 12}
			if name == "nullable-omitted" {
				v.PriceID = nil
			}
			release := map[string]any{"hold_id": v.HoldID, "usage_request_id": v.UsageID, "wallet_id": v.WalletID, "status": "RELEASED", "max_microcredits": int64(500)}
			ack := map[string]any{"hold_id": v.HoldID, "usage_request_id": v.UsageID, "wallet_id": v.WalletID, "status": "ACTIVE", "max_microcredits": int64(500), "expires_at": time.Now().Add(-time.Hour)}
			if v.PriceID != nil {
				release["price_version_id"] = v.PriceID
				ack["price_version_id"] = v.PriceID
			}
			if name == "release-wrong-hold" {
				release["hold_id"] = uuid.New()
			}
			if name == "release-smaller" {
				release["max_microcredits"] = 11
			}
			if name == "proof-wrong-wallet" {
				ack["wallet_id"] = uuid.New()
			}
			if name == "proof-wrong-price" {
				ack["price_version_id"] = uuid.New()
			}
			if name == "proof-wrong-maximum" {
				ack["max_microcredits"] = 501
			}
			if name == "proof-actual" {
				ack["actual_microcredits"] = 0
			}
			if name == "proof-missing-maximum" {
				delete(ack, "max_microcredits")
			}
			if name == "proof-null-maximum" {
				ack["max_microcredits"] = nil
			}
			if name == "proof-missing-expiry" {
				delete(ack, "expires_at")
			}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/internal/v1/holds/release" {
					if name == "release-201" {
						w.WriteHeader(201)
					}
					if name == "release-503" {
						w.WriteHeader(503)
					}
					_ = json.NewEncoder(w).Encode(release)
					return
				}
				calls++
				var body struct {
					Usage      uuid.UUID `json:"usage_request_id"`
					Hold       uuid.UUID `json:"hold_id"`
					Operation  uuid.UUID `json:"operation_id"`
					Lease      int       `json:"lease_seconds"`
					Additional int64     `json:"additional_microcredits"`
				}
				if json.NewDecoder(r.Body).Decode(&body) != nil || body.Usage != v.UsageID || body.Hold != v.HoldID || body.Operation != finalUsageReservationOperationID(v.UsageID, v.HoldID) || body.Lease != 900 || body.Additional != 488 {
					t.Error("proof changed original operation facts")
				}
				if name == "proof-201" {
					w.WriteHeader(201)
				}
				if name == "proof-503" {
					w.WriteHeader(503)
				}
				if name == "wrong-fingerprint" || name == "wrong-lease" {
					w.WriteHeader(409)
				}
				if name == "proof-malformed" {
					_, _ = w.Write([]byte(`{`))
					return
				}
				_ = json.NewEncoder(w).Encode(ack)
			}))
			defer server.Close()
			client, err := NewHTTPBillingClient(server.URL, "controlled")
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := client.ReleaseCapturedReference(context.Background(), v)
			want := name == "exact" || name == "nullable-omitted"
			if (err == nil) != want {
				t.Fatal("proof accepted wrong boundary", name, err)
			}
			if len(name) >= 8 && name[:8] == "release-" {
				if calls != 0 {
					t.Fatal("unconfirmed release sent proof")
				}
			} else if calls != 1 {
				t.Fatal("proof repeated", calls)
			}
			if want {
				if !capturedReceiptMatches(receipt, v) {
					t.Fatal("finisher rejected exact proof")
				}
				receipt.Extension.LeaseSeconds++
				if capturedReceiptMatches(receipt, v) {
					t.Fatal("finisher accepted modified lease")
				}
			}
		})
	}
}
