package token

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

// Production facade handlers and clients use controlled HTTP authorities. This
// does not claim provider payments, Token invocation or deployed organization UI.
func TestOrganizationFinanceAuthorityAndWalletScope(t *testing.T) {
	user, org, other, wallet, order := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, tc := range []struct {
		name, kind, method, suffix, body string
		want                             int
	}{
		{"balance", "valid", "GET", "/balance", "", 200},
		{"foreign-ensure-owner", "foreign-ensure", "GET", "/balance", "", 503},
		{"personal-ensure-owner", "personal-ensure", "GET", "/balance", "", 503},
		{"foreign-balance-owner", "foreign-balance", "GET", "/balance", "", 503},
		{"denied", "denied", "GET", "/balance", "", 403},
		{"wrong-authorized-user", "foreign-user", "GET", "/balance", "", 403},
		{"organization-products", "valid", "GET", "/recharge/products", "", 200},
		{"organization-capabilities", "valid", "GET", "/payment-capabilities", "", 200},
		{"denied-products", "denied", "GET", "/recharge/products", "", 403},
		{"denied-capabilities", "denied", "GET", "/payment-capabilities", "", 403},
		{"order", "valid", "GET", "/recharge/orders/" + order.String(), "", 200},
		{"history", "valid", "GET", "/recharge/orders?cursor=cGFnZQ", "", 200},
		{"denied-history", "denied", "GET", "/recharge/orders", "", 403},
		{"foreign-history-wallet", "foreign-history-wallet", "GET", "/recharge/orders", "", 502},
		{"foreign-history-requester", "foreign-history-requester", "GET", "/recharge/orders", "", 502},
		{"duplicate-history-orders", "duplicate-history-orders", "GET", "/recharge/orders", "", 502},
		{"forged-history-wallet", "valid", "GET", "/recharge/orders?wallet_id=" + other.String(), "", 400},
		{"duplicate-history-cursor", "valid", "GET", "/recharge/orders?cursor=a&cursor=b", "", 400},
		{"invalid-history-cursor", "valid", "GET", "/recharge/orders?cursor=invalid%20cursor", "", 400},
		{"foreign-order-org", "foreign-order-org", "GET", "/recharge/orders/" + order.String(), "", 404},
		{"foreign-order-wallet", "foreign-order-wallet", "GET", "/recharge/orders/" + order.String(), "", 404},
		{"create", "valid", "POST", "/recharge/orders", `{"product_id":"owned-product","channel":"alipay"}`, 201},
		{"client-wallet", "valid", "POST", "/recharge/orders", `{"product_id":"owned-product","channel":"alipay","wallet_id":"` + other.String() + `"}`, 400},
		{"client-beneficiary", "valid", "POST", "/recharge/orders", `{"product_id":"owned-product","channel":"alipay","beneficiary_organization_id":"` + other.String() + `"}`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ensure, create atomic.Int32
			account := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/internal/v1/authorization/check":
					var in struct {
						Permission     string     `json:"permission"`
						OrganizationID *uuid.UUID `json:"target_organization_id"`
					}
					if json.NewDecoder(r.Body).Decode(&in) != nil || in.OrganizationID == nil || *in.OrganizationID != org || !strings.HasPrefix(in.Permission, "org.") {
						t.Error("wrong organization permission target")
					}
					id := user
					if tc.kind == "foreign-user" {
						id = other
					}
					w.Header().Set("X-Authorization-Version", "7")
					_ = json.NewEncoder(w).Encode(map[string]any{"allowed": tc.kind != "denied", "user_id": id})
				case "/v1/me/authorization-context":
					_ = json.NewEncoder(w).Encode(map[string]any{"user_id": user, "context_type": "ORGANIZATION", "organization_id": org, "organization_membership_version": 3, "membership_version": 5, "context_version": 2, "authorization_version": 7, "roles": []string{"OWNER"}, "mobile": "+8613800138000", "mobile_verified": true})
				case "/v1/me/commerce-customer-mapping":
					_ = json.NewEncoder(w).Encode(map[string]string{"status": "READY"})
				default:
					t.Error("unexpected authority route")
					http.NotFound(w, r)
				}
			}))
			defer account.Close()
			billing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer owned-service" {
					t.Error("lost Billing service identity")
				}
				switch r.URL.Path {
				case "/internal/v1/wallets/ensure":
					ensure.Add(1)
					var in struct {
						Owner uuid.UUID `json:"owner_id"`
						Type  string    `json:"owner_type"`
					}
					if json.NewDecoder(r.Body).Decode(&in) != nil || in.Owner != org || in.Type != "ORGANIZATION" {
						t.Error("client-controlled wallet owner")
					}
					owner, typ := org, "ORGANIZATION"
					if tc.kind == "foreign-ensure" {
						owner = other
					}
					if tc.kind == "personal-ensure" {
						typ = "PERSONAL"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"wallet_id": wallet, "owner_type": typ, "owner_id": owner, "status": "ACTIVE", "version": 1})
				case "/internal/v1/wallets/" + wallet.String() + "/balance":
					owner := org
					if tc.kind == "foreign-balance" {
						owner = other
					}
					fmt.Fprintf(w, `{"wallet_id":%q,"owner_type":"ORGANIZATION","owner_id":%q,"status":"ACTIVE","version":1,"posted_microcredits":9007199254740993,"reserved_microcredits":0,"available_microcredits":9007199254740993}`, wallet.String(), owner.String())
				case "/internal/v1/recharge/orders/" + order.String():
					owner, scopeWallet := org, wallet
					if tc.kind == "foreign-order-org" {
						owner = other
					}
					if tc.kind == "foreign-order-wallet" {
						scopeWallet = other
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"order_id": order, "requester_id": user, "wallet_id": scopeWallet, "beneficiary_organization_id": owner})
				case "/v1/recharge/products":
					if tc.kind == "denied" {
						t.Error("denied organization accessed product catalog")
					}
					_, _ = fmt.Fprint(w, `{"items":[{"product_id":"owned-product","amount_fen":9007199254740993,"channels":["ALIPAY"]}]}`)
				case "/v1/payment-capabilities":
					if tc.kind == "denied" {
						t.Error("denied organization accessed payment capabilities")
					}
					_, _ = fmt.Fprint(w, `{"channels":["ALIPAY"]}`)
				case "/internal/v1/recharge/orders":
					if r.Method == http.MethodGet {
						if r.URL.Query().Get("requester_id") != user.String() || r.URL.Query().Get("wallet_id") != wallet.String() || r.URL.Query().Get("limit") != "50" {
							t.Error("order history lost its resolved payer/wallet boundary")
						}
						if tc.name == "history" && r.URL.Query().Get("cursor") != "cGFnZQ" {
							t.Error("history cursor was not forwarded")
						}
						requester, scopeWallet := user, wallet
						if tc.kind == "foreign-history-requester" {
							requester = other
						}
						if tc.kind == "foreign-history-wallet" {
							scopeWallet = other
						}
						items := []map[string]any{{"order_id": order, "amount_fen": int64(9007199254740993)}}
						if tc.kind == "duplicate-history-orders" {
							items = append(items, items[0])
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"requester_id": requester, "wallet_id": scopeWallet, "items": items, "next_cursor": nil})
						return
					}
					create.Add(1)
					var in struct {
						Requester, Wallet, Org   uuid.UUID
						Channel, Mobile, Mapping string
					}
					var body map[string]json.RawMessage
					if json.NewDecoder(r.Body).Decode(&body) != nil {
						t.Error("invalid creation wire")
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					_ = json.Unmarshal(body["requester_id"], &in.Requester)
					_ = json.Unmarshal(body["wallet_id"], &in.Wallet)
					_ = json.Unmarshal(body["beneficiary_organization_id"], &in.Org)
					_ = json.Unmarshal(body["channel"], &in.Channel)
					_ = json.Unmarshal(body["mobile"], &in.Mobile)
					_ = json.Unmarshal(body["commerce_customer_status"], &in.Mapping)
					if in.Requester != user || in.Wallet != wallet || in.Org != org || in.Channel != "ALIPAY" || in.Mobile != "+8613800138000" || in.Mapping != "READY" || r.Header.Get("Idempotency-Key") != "owned-org-create-intent" {
						t.Error("creation lost immutable organization payment facts")
					}
					w.WriteHeader(201)
					_ = json.NewEncoder(w).Encode(map[string]any{"order_id": order, "wallet_id": wallet, "beneficiary_organization_id": org, "requester_id": user})
				default:
					t.Error("unexpected Billing route")
					http.NotFound(w, r)
				}
			}))
			defer billing.Close()
			contexts, err := NewHTTPContextResolver(account.URL)
			if err != nil {
				t.Fatal(err)
			}
			wallets, err := NewHTTPWalletResolver(billing.URL, "owned-service")
			if err != nil {
				t.Fatal(err)
			}
			client, err := NewHTTPBillingClient(billing.URL, "owned-service")
			if err != nil {
				t.Fatal(err)
			}
			auth, err := authorization.NewClient(account.URL, "owned-service")
			if err != nil {
				t.Fatal(err)
			}
			h := NewService(contexts, wallets, client).WithManagement(nil, nil, ProviderURLPolicy{}, "owned-service", nil).WithAuthorizer(auth).Handler(func(context.Context, string) (identity.Principal, error) {
				return identity.Principal{Issuer: "owned", Subject: "owner"}, nil
			})
			r := httptest.NewRequest(tc.method, "/v1/organizations/"+org.String()+tc.suffix, strings.NewReader(tc.body))
			r.Header.Set("Authorization", "Bearer owned-user")
			r.Header.Set("Idempotency-Key", "owned-org-create-intent")
			out := httptest.NewRecorder()
			h.ServeHTTP(out, r)
			if out.Code != tc.want {
				t.Errorf("HTTP=%d want=%d", out.Code, tc.want)
			}
			if tc.want == 403 && ensure.Load() != 0 {
				t.Error("denied authority reached Billing")
			}
			if tc.method == "POST" && tc.want == 400 && create.Load() != 0 {
				t.Error("invalid client facts reached payment creation")
			}
			if tc.name == "balance" && !strings.Contains(out.Body.String(), "9007199254740993") {
				t.Error("organization amount lost exact int64 bytes")
			}
			if tc.name == "history" && (!strings.Contains(out.Body.String(), "9007199254740993") || out.Header().Get("Cache-Control") != "no-store" || !strings.Contains(out.Body.String(), org.String())) {
				t.Error("organization history lost exact monetary bytes, owner binding or no-store policy")
			}
		})
	}
}
