package token

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/portal"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

// Uses real facade handlers and HTTP clients against controlled authority
// servers. Provider payment processing and database effects are separate tests.
func TestPaymentSourceActualPersonalCreateRejectsForgedForwarding(t *testing.T) {
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
					if got := r.Header.Get("X-Pinvou-Client-Source"); got != "198.51.100.15" {
						t.Errorf("untrusted forwarding entered authenticated Billing admission: source=%q, want actual peer 198.51.100.15", got)
					}
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
			request.RemoteAddr = "198.51.100.15:40100"
			request.Header.Set("X-Forwarded-For", "203.0.113.200")
			request.Header.Set("X-Pinvou-Client-Source", "203.0.113.201")
			request.Header.Set("X-Pinvou-Client-Source-Signature", strings.Repeat("00", 32))
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

// Uses real facade handlers and HTTP clients against controlled authority
// servers. Provider payment processing and database effects are separate tests.
func TestPaymentSourceActualSignedPortalCreate(t *testing.T) {
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
					if got := r.Header.Get("X-Pinvou-Client-Source"); got != "198.51.100.15" {
						t.Errorf("untrusted forwarding entered authenticated Billing admission: source=%q, want actual peer 198.51.100.15", got)
					}
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
			service := NewService(contexts, wallets, client).WithAuthorizer(authorizer).WithAdminProxyHMACKey([]byte("controlled-payment-source-key-32bytes"))
			handler := service.Handler(func(_ context.Context, token string) (identity.Principal, error) {
				if token != "fixture-user" {
					return identity.Principal{}, errors.New("invalid fixture identity")
				}
				return identity.Principal{Issuer: "fixture", Subject: "fixture"}, nil
			})
			gateway := httptest.NewServer(handler)
			defer gateway.Close()
			parse := func(raw string) *url.URL {
				u, err := url.Parse(raw)
				if err != nil {
					t.Fatal(err)
				}
				return u
			}
			session := portal.Session{ID: "controlled-payment-session", CSRFToken: "controlled-payment-csrf-at-least-32-bytes", AccessToken: "fixture-user", AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(2 * time.Hour)}
			cfg := portal.Config{Environment: "test", ClientID: "portal", IssuerURL: parse("https://identity.example"), JWKSURL: parse("https://identity.example/keys"), PublicURL: parse("https://portal.example"), AuthorizationURL: parse("https://identity.example/auth"), TokenURL: parse("https://identity.example/token"), AccountURL: parse(account.URL), BillingURL: parse(billing.URL), TokenGatewayURL: parse(gateway.URL), SecureCookies: true, TokenAdminProxyHMACKey: []byte("controlled-payment-source-key-32bytes")}
			p, err := portal.NewService(cfg, retainedF05SignedPortalStore{session: session})
			if err != nil {
				t.Fatal(err)
			}
			handler = p.Handler(nil)
			request := httptest.NewRequest(http.MethodPost, "https://portal.example/api/token/v1/recharge/orders", strings.NewReader(tc.body))
			request.Header.Set("Origin", "https://portal.example")
			request.Header.Set("X-CSRF-Token", session.CSRFToken)
			request.AddCookie(&http.Cookie{Name: "__Host-portal_session", Value: session.ID})
			request.AddCookie(&http.Cookie{Name: "__Host-portal_csrf", Value: session.CSRFToken})
			request.RemoteAddr = "198.51.100.15:40100"
			request.Header.Set("X-Forwarded-For", "203.0.113.200")
			request.Header.Set("X-Pinvou-Client-Source", "203.0.113.201")
			request.Header.Set("X-Pinvou-Client-Source-Signature", strings.Repeat("00", 32))
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
