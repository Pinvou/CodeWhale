package token

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

// Uses real facade handlers and HTTP clients against controlled authority
// servers. Provider payment processing and database effects are separate tests.
func TestPersonalFinanceCreationBindsAuthorityAndRejectsClientOwnerFacts(t *testing.T) {
	user, wallet := uuid.New(), uuid.New()
	for _, tc := range []struct {
		name, body        string
		allowed, verified bool
		want              int
		wantCreates       int32
	}{
		{"create", `{"product_id":"fixture-plan","channel":"alipay"}`, true, true, 201, 1},
		{"denied", `{"product_id":"fixture-plan","channel":"ALIPAY"}`, false, true, 403, 0},
		{"unverified", `{"product_id":"fixture-plan","channel":"ALIPAY"}`, true, false, 409, 0},
		{"client-requester", `{"product_id":"fixture-plan","channel":"ALIPAY","requester_id":"` + uuid.NewString() + `"}`, true, true, 400, 0},
		{"client-mobile", `{"product_id":"fixture-plan","channel":"ALIPAY","mobile":"not-authoritative"}`, true, true, 400, 0},
		{"second-json", `{"product_id":"fixture-plan","channel":"ALIPAY"} {}`, true, true, 400, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var creates atomic.Int32
			account := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/internal/v1/authorization/check":
					var check struct{ Permission string }
					_ = json.NewDecoder(r.Body).Decode(&check)
					if check.Permission != "personal.recharge.create" {
						t.Error("wrong finance permission")
					}
					w.Header().Set("X-Authorization-Version", "1")
					_ = json.NewEncoder(w).Encode(map[string]any{"allowed": tc.allowed, "user_id": user})
				case "/v1/me/authorization-context":
					if r.Header.Get("Authorization") != "Bearer fixture-user" {
						t.Error("lost user authority")
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"user_id": user, "context_type": "PERSONAL", "context_version": 1, "authorization_version": 1, "mobile": "+8613800138000", "mobile_verified": tc.verified})
				case "/v1/me/commerce-customer-mapping":
					_ = json.NewEncoder(w).Encode(map[string]string{"status": "READY"})
				default:
					t.Error("unexpected authority route")
					http.NotFound(w, r)
				}
			}))
			defer account.Close()
			billing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer fixture-service" {
					t.Error("lost service authority")
				}
				switch r.URL.Path {
				case "/internal/v1/wallets/ensure":
					var body struct {
						Owner uuid.UUID `json:"owner_id"`
						Type  string    `json:"owner_type"`
					}
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body.Owner != user || body.Type != "PERSONAL" {
						t.Error("wrong wallet owner")
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"wallet_id": wallet, "owner_id": user, "owner_type": "PERSONAL", "status": "ACTIVE", "version": 1})
				case "/internal/v1/recharge/orders":
					creates.Add(1)
					var body struct {
						User    uuid.UUID `json:"requester_id"`
						Wallet  uuid.UUID `json:"wallet_id"`
						Mobile  string    `json:"mobile"`
						Channel string    `json:"channel"`
						Mapping string    `json:"commerce_customer_status"`
					}
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body.User != user || body.Wallet != wallet || body.Mobile != "+8613800138000" || body.Channel != "ALIPAY" || body.Mapping != "READY" || r.Header.Get("Idempotency-Key") != "fixture-logical-operation" {
						t.Error("payment creation lost immutable authority or logical key")
					}
					w.WriteHeader(201)
					_, _ = io.WriteString(w, `{"order_id":"`+uuid.NewString()+`","requester_id":"`+user.String()+`","wallet_id":"`+wallet.String()+`","amount_fen":9007199254740993}`)
				default:
					t.Error("unexpected Billing route")
					http.NotFound(w, r)
				}
			}))
			defer billing.Close()
			contexts, _ := NewHTTPContextResolver(account.URL)
			wallets, _ := NewHTTPWalletResolver(billing.URL, "fixture-service")
			client, _ := NewHTTPBillingClient(billing.URL, "fixture-service")
			authorizer, _ := authorization.NewClient(account.URL, "fixture-service")
			service := NewService(contexts, wallets, client).WithAuthorizer(authorizer)
			handler := service.Handler(func(_ context.Context, token string) (identity.Principal, error) {
				if token != "fixture-user" {
					return identity.Principal{}, errors.New("invalid fixture identity")
				}
				return identity.Principal{Issuer: "fixture", Subject: "fixture"}, nil
			})
			request := httptest.NewRequest(http.MethodPost, "/v1/recharge/orders", strings.NewReader(tc.body))
			request.Header.Set("Authorization", "Bearer fixture-user")
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Idempotency-Key", "fixture-logical-operation")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.want || creates.Load() != tc.wantCreates {
				t.Fatalf("status %d creates %d; expected %d/%d", response.Code, creates.Load(), tc.want, tc.wantCreates)
			}
			if response.Code == 201 && (!strings.Contains(response.Body.String(), "9007199254740993") || response.Header().Get("Cache-Control") != "no-store") {
				t.Fatal("finance facade changed exact response facts or caching")
			}
		})
	}
}

func TestPersonalFinanceClientRejectsIncompleteOrOversizeJSONAndPreservesRetry(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		reject     bool
	}{
		{"valid", `{"items":[],"catalog_version":0}`, 200, false},
		{"second-value", `{"items":[]} {}`, 200, true},
		{"truncated", `{"items":[`, 200, true},
		{"oversize", `{"items":[],"extension":"` + strings.Repeat("x", 2<<20) + `"}`, 200, true},
		{"rate-limited", `{"code":"PAYMENT_RATE_LIMITED"}`, 429, true},
		{"wrong-success-status", `{"items":[]}`, 201, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "10")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer upstream.Close()
			client, _ := NewHTTPBillingClient(upstream.URL, "fixture-service")
			raw, err := client.Products(context.Background())
			if (err != nil) != tc.reject {
				t.Fatal("unexpected financial reply acceptance")
			}
			if !tc.reject && string(raw) != tc.body {
				t.Fatal("financial facts changed during proxying")
			}
			if tc.status == 429 {
				var problem *BillingHTTPError
				if !errors.As(err, &problem) || problem.RetryAfter != "10" || problem.Status != 429 {
					t.Fatal("lost rate limit retry facts")
				}
			}
		})
	}
}
