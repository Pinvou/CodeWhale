package token

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestIndependentPersonalBalanceFacts(t *testing.T) {
	user, other, wallet, foreign := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, tc := range []struct {
		name     string
		badOwner bool
		bodyKind string
		want     int
	}{
		{"actual-wallet", false, "valid", 200}, {"unknown-extension", false, "extension", 200}, {"foreign-ensure-owner", true, "valid", 503}, {"foreign-balance-wallet", false, "foreign", 503}, {"null-balance", false, "null", 503}, {"array-balance", false, "array", 503}, {"missing-wallet-balance", false, "missing", 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/internal/v1/authorization/check":
					w.Header().Set("X-Authorization-Version", "1")
					json.NewEncoder(w).Encode(map[string]any{"allowed": true, "user_id": user})
				case "/v1/me/authorization-context":
					json.NewEncoder(w).Encode(map[string]any{"user_id": user, "context_type": "PERSONAL", "context_version": 1, "authorization_version": 1, "personal_roles": []string{"PERSONAL_USER"}, "personal_permissions": []string{"personal.billing.read"}})
				default:
					http.NotFound(w, r)
				}
			}))
			defer account.Close()
			var reads atomic.Int32
			billing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer fixture-service" {
					t.Error("lost service authority")
				}
				if r.URL.Path == "/internal/v1/wallets/ensure" {
					id, owner := wallet, user
					if tc.badOwner {
						id, owner = foreign, other
					}
					w.WriteHeader(201)
					json.NewEncoder(w).Encode(map[string]any{"wallet_id": id, "owner_type": "PERSONAL", "owner_id": owner, "status": "ACTIVE", "version": 1})
					return
				}
				reads.Add(1)
				id, owner := wallet, user
				if tc.badOwner {
					id, owner = foreign, other
				}
				if tc.bodyKind == "foreign" {
					id = foreign
				}
				switch tc.bodyKind {
				case "null":
					fmt.Fprint(w, "null")
				case "array":
					fmt.Fprint(w, "[]")
				case "missing":
					fmt.Fprint(w, "{}")
				default:
					body := map[string]any{"wallet_id": id, "owner_type": "PERSONAL", "owner_id": owner, "status": "ACTIVE", "posted_microcredits": 12345, "reserved_microcredits": 0, "receivable_microcredits": 0, "available_microcredits": 12345, "financial_status": "CLEAR", "version": 1}
					if tc.bodyKind == "extension" {
						body["future_metadata"] = "allowed"
					}
					json.NewEncoder(w).Encode(body)
				}
			}))
			defer billing.Close()
			contexts, _ := NewHTTPContextResolver(account.URL)
			wallets, _ := NewHTTPWalletResolver(billing.URL, "fixture-service")
			balances, _ := NewHTTPBillingClient(billing.URL, "fixture-service")
			auth, _ := authorization.NewClient(account.URL, "fixture-service")
			s := NewService(contexts, wallets, balances).WithAuthorizer(auth)
			h := s.Handler(func(context.Context, string) (identity.Principal, error) {
				return identity.Principal{Issuer: "fixture", Subject: "user"}, nil
			})
			req := httptest.NewRequest("GET", "/v1/me/balance", nil)
			req.Header.Set("Authorization", "Bearer fixture-user")
			out := httptest.NewRecorder()
			h.ServeHTTP(out, req)
			if out.Code != tc.want {
				t.Errorf("status=%d want=%d balanceReads=%d", out.Code, tc.want, reads.Load())
			}
			if tc.badOwner && reads.Load() != 0 {
				t.Error("read balance after foreign-owner ensure acknowledgement")
			}
		})
	}
}
