package account

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func TestRegistrationWalletAuthorityRequiresReceiptAndOwnedWallet(t *testing.T) {
	userID, operationID, walletID, claimID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	input := RegistrationWalletProvisionRequest{UserID: userID, RegistrationOperationID: operationID, FingerprintKeyID: "registration-bonus-v1", MobileFingerprint: strings.Repeat("31", 32)}
	baseReceipt := func() map[string]any {
		return map[string]any{"registration_operation_id": operationID, "user_id": userID, "wallet_id": walletID, "claim_id": claimID, "policy_revision": int64(1), "outcome": "GRANTED", "granted_microcredits": int64(9007199254750000), "created_at": "2026-10-08T00:00:00.123456Z"}
	}
	baseWallet := func() map[string]any {
		return map[string]any{"wallet_id": walletID, "owner_type": "PERSONAL", "owner_id": userID, "status": "ACTIVE", "version": int64(1)}
	}
	type scenario struct {
		name          string
		receiptStatus int
		receipt       func(map[string]any) string
		walletStatus  int
		wallet        func(map[string]any) string
		wantError     bool
		wantEnsure    int
	}
	encode := func(value map[string]any) string {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	receiptField := func(name string, value any) func(map[string]any) string {
		return func(receipt map[string]any) string { receipt[name] = value; return encode(receipt) }
	}
	walletField := func(name string, value any) func(map[string]any) string {
		return func(wallet map[string]any) string { wallet[name] = value; return encode(wallet) }
	}
	cases := []scenario{
		{name: "granted-int64-exact", wantEnsure: 1},
		{name: "replayed-receipt-200", receiptStatus: 200, wantEnsure: 1},
		{name: "disabled-is-valid-zero", receipt: func(r map[string]any) string {
			r["outcome"] = "NOT_GRANTED_DISABLED"
			r["granted_microcredits"] = int64(0)
			return encode(r)
		}, wantEnsure: 1},
		{name: "zero-policy-is-valid-zero", receipt: func(r map[string]any) string {
			r["outcome"] = "NOT_GRANTED_ZERO"
			r["granted_microcredits"] = int64(0)
			return encode(r)
		}, wantEnsure: 1},
		{name: "already-claimed-is-valid-zero", receipt: func(r map[string]any) string {
			r["outcome"] = "ALREADY_CLAIMED"
			r["granted_microcredits"] = int64(0)
			return encode(r)
		}, wantEnsure: 1},
		{name: "unknown-extension-compatible", receipt: func(r map[string]any) string { r["extension"] = "retained"; return encode(r) }, wallet: walletField("extension", true), wantEnsure: 1},
		{name: "bare-200", receiptStatus: 200, receipt: func(map[string]any) string { return "" }, wantError: true},
		{name: "null-receipt", receipt: func(map[string]any) string { return "null" }, wantError: true},
		{name: "partial-receipt", receipt: func(map[string]any) string { return "{}" }, wantError: true},
		{name: "trailing-receipt", receipt: func(r map[string]any) string { return encode(r) + " {}" }, wantError: true},
		{name: "oversized-receipt", receipt: func(r map[string]any) string { return encode(r) + strings.Repeat(" ", 64<<10) }, wantError: true},
		{name: "foreign-user", receipt: receiptField("user_id", uuid.New()), wantError: true},
		{name: "foreign-operation", receipt: receiptField("registration_operation_id", uuid.New()), wantError: true},
		{name: "nil-wallet", receipt: receiptField("wallet_id", uuid.Nil), wantError: true},
		{name: "nil-claim", receipt: receiptField("claim_id", uuid.Nil), wantError: true},
		{name: "missing-revision", receipt: func(r map[string]any) string { delete(r, "policy_revision"); return encode(r) }, wantError: true},
		{name: "null-revision", receipt: receiptField("policy_revision", nil), wantError: true},
		{name: "invalid-revision", receipt: receiptField("policy_revision", 0), wantError: true},
		{name: "missing-amount", receipt: func(r map[string]any) string { delete(r, "granted_microcredits"); return encode(r) }, wantError: true},
		{name: "null-amount", receipt: receiptField("granted_microcredits", nil), wantError: true},
		{name: "fractional-amount", receipt: receiptField("granted_microcredits", 1.5), wantError: true},
		{name: "overflow-amount", receipt: receiptField("granted_microcredits", json.Number("9223372036854775808")), wantError: true},
		{name: "unknown-outcome", receipt: receiptField("outcome", "PENDING"), wantError: true},
		{name: "granted-zero", receipt: receiptField("granted_microcredits", 0), wantError: true},
		{name: "granted-not-whole-points", receipt: receiptField("granted_microcredits", 10001), wantError: true},
		{name: "zero-outcome-nonzero", receipt: receiptField("outcome", "ALREADY_CLAIMED"), wantError: true},
		{name: "missing-created-at", receipt: func(r map[string]any) string { delete(r, "created_at"); return encode(r) }, wantError: true},
		{name: "null-created-at", receipt: receiptField("created_at", nil), wantError: true},
		{name: "zero-created-at", receipt: receiptField("created_at", "0001-01-01T00:00:00Z"), wantError: true},
		{name: "unknown-202-not-complete", receiptStatus: 202, wantError: true},
		{name: "bare-204-not-complete", receiptStatus: 204, receipt: func(map[string]any) string { return "" }, wantError: true},
		{name: "wallet-foreign-owner", wallet: walletField("owner_id", uuid.New()), wantError: true, wantEnsure: 1},
		{name: "wallet-org-not-personal", wallet: walletField("owner_type", "ORG"), wantError: true, wantEnsure: 1},
		{name: "wallet-not-active", wallet: walletField("status", "DISABLED"), wantError: true, wantEnsure: 1},
		{name: "wallet-version-missing", wallet: func(r map[string]any) string { delete(r, "version"); return encode(r) }, wantError: true, wantEnsure: 1},
		{name: "wallet-id-mismatch", wallet: walletField("wallet_id", uuid.New()), wantError: true, wantEnsure: 1},
		{name: "wallet-unavailable-after-durable-gift", walletStatus: 503, wantError: true, wantEnsure: 1},
		{name: "wallet-null-not-ack", wallet: func(map[string]any) string { return "null" }, wantError: true, wantEnsure: 1},
		{name: "wallet-trailing-not-ack", wallet: func(r map[string]any) string { return encode(r) + " {}" }, wantError: true, wantEnsure: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var calls []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				calls = append(calls, r.URL.Path)
				if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Authorization") != "Bearer controlled-service-token" {
					t.Error("authority method or headers changed")
				}
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/internal/v1/registration-bonus-grants":
					var sent RegistrationWalletProvisionRequest
					if json.Unmarshal(raw, &sent) != nil || sent != input || r.Header.Get("Idempotency-Key") != "registration-bonus:"+operationID.String() {
						t.Error("registration frozen request facts changed")
					}
					status := tc.receiptStatus
					if status == 0 {
						status = 201
					}
					w.WriteHeader(status)
					body := encode(baseReceipt())
					if tc.receipt != nil {
						body = tc.receipt(baseReceipt())
					}
					_, _ = io.WriteString(w, body)
				case "/internal/v1/wallets/ensure":
					var sent struct {
						OwnerType string    `json:"owner_type"`
						OwnerID   uuid.UUID `json:"owner_id"`
					}
					if json.Unmarshal(raw, &sent) != nil || sent.OwnerType != "PERSONAL" || sent.OwnerID != userID || r.Header.Get("Idempotency-Key") != "personal-wallet:"+userID.String() {
						t.Error("wallet authority request changed")
					}
					status := tc.walletStatus
					if status == 0 {
						status = 200
					}
					w.WriteHeader(status)
					body := encode(baseWallet())
					if tc.wallet != nil {
						body = tc.wallet(baseWallet())
					}
					_, _ = io.WriteString(w, body)
				default:
					t.Error("unexpected authority endpoint")
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			p, err := NewHTTPWalletProvisioner(server.URL, "controlled-service-token")
			if err != nil {
				t.Fatal(err)
			}
			err = p.ProvisionRegistrationWallet(context.Background(), input)
			if tc.wantError {
				if !errors.Is(err, ErrWalletProvisioning) {
					t.Fatalf("malformed or mismatched facts acknowledged: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			want := []string{"/internal/v1/registration-bonus-grants"}
			if tc.wantEnsure == 1 {
				want = append(want, "/internal/v1/wallets/ensure")
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("authority side effects/order = %v; want %v", calls, want)
			}
		})
	}
}

func TestRegistrationWalletAuthorityRejectsInvalidJobBeforeHTTP(t *testing.T) {
	input := RegistrationWalletProvisionRequest{UserID: uuid.New(), RegistrationOperationID: uuid.New(), FingerprintKeyID: "registration-bonus-v1", MobileFingerprint: strings.Repeat("31", 32)}
	cases := []struct {
		name   string
		change func(*RegistrationWalletProvisionRequest)
	}{
		{"nil-user", func(r *RegistrationWalletProvisionRequest) { r.UserID = uuid.Nil }},
		{"nil-operation", func(r *RegistrationWalletProvisionRequest) { r.RegistrationOperationID = uuid.Nil }},
		{"missing-key", func(r *RegistrationWalletProvisionRequest) { r.FingerprintKeyID = "" }},
		{"invalid-fingerprint", func(r *RegistrationWalletProvisionRequest) { r.MobileFingerprint = "not-fingerprint" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("invalid job made an authority request") }))
			defer server.Close()
			p, err := NewHTTPWalletProvisioner(server.URL, "controlled-service-token")
			if err != nil {
				t.Fatal(err)
			}
			changed := input
			tc.change(&changed)
			if !errors.Is(p.ProvisionRegistrationWallet(context.Background(), changed), ErrWalletProvisioning) {
				t.Fatal("invalid job accepted")
			}
		})
	}
}
