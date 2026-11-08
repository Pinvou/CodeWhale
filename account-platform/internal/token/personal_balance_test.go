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

// The fixture uses the actual gateway handler and its HTTP clients. It proves
// transport/identity boundaries; it is not a real Billing database acceptance.
func TestPersonalBalanceUsesAuthorizedPersonalWalletAndRejectsFutureRoutes(t *testing.T) {
	userID, walletID := uuid.New(), uuid.New()
	for _, test := range []struct {
		name            string
		allowed         bool
		resolvedUser    uuid.UUID
		contextType     string
		organizationID  *uuid.UUID
		contextVersion  int
		billingStatus   int
		wantStatus      int
		wantBillingHits int32
	}{
		{"personal-balance", true, userID, "PERSONAL", nil, 3, 200, 200, 2},
		{"permission-denied", false, userID, "PERSONAL", nil, 3, 200, 403, 0},
		{"wrong-user", true, uuid.New(), "PERSONAL", nil, 3, 200, 403, 0},
		{"organization-context", true, userID, "ORGANIZATION", &userID, 3, 200, 403, 0},
		{"personal-with-organization", true, userID, "PERSONAL", &userID, 3, 200, 403, 0},
		{"missing-context-version", true, userID, "PERSONAL", nil, 0, 200, 403, 0},
		{"billing-failure", true, userID, "PERSONAL", nil, 3, 503, 503, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			var billingHits atomic.Int32
			account := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/internal/v1/authorization/check":
					if r.Header.Get("Authorization") != "Bearer fixture-service-token" || r.Method != http.MethodPost {
						t.Error("authorization request did not use the service authority")
					}
					var check struct {
						Issuer, Subject, Permission string
					}
					if err := json.NewDecoder(r.Body).Decode(&check); err != nil || check.Issuer != "fixture" || check.Subject != "subject" || check.Permission != "personal.billing.read" {
						t.Error("the balance authorization facts are not bound to the principal")
					}
					w.Header().Set("X-Authorization-Version", "4")
					_ = json.NewEncoder(w).Encode(map[string]any{"allowed": test.allowed, "user_id": userID})
				case "/v1/me/authorization-context":
					if r.Header.Get("Authorization") != "Bearer fixture-user-token" || r.Method != http.MethodGet {
						t.Error("context resolution did not preserve the user token")
					}
					_ = json.NewEncoder(w).Encode(map[string]any{
						"user_id": test.resolvedUser, "context_type": test.contextType, "organization_id": test.organizationID,
						"context_version": test.contextVersion, "authorization_version": 4,
						"personal_roles": []string{"PERSONAL_USER"}, "personal_permissions": []string{"personal.billing.read"},
					})
				default:
					t.Errorf("unexpected Account request %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer account.Close()
			billing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				billingHits.Add(1)
				if r.Header.Get("Authorization") != "Bearer fixture-service-token" {
					t.Error("Billing did not receive its service authority")
				}
				switch r.URL.Path {
				case "/internal/v1/wallets/ensure":
					var owner struct {
						OwnerType string    `json:"owner_type"`
						OwnerID   uuid.UUID `json:"owner_id"`
					}
					if err := json.NewDecoder(r.Body).Decode(&owner); err != nil || owner.OwnerType != "PERSONAL" || owner.OwnerID != userID {
						t.Error("wallet resolution was not bound to the authorized personal owner")
					}
					w.WriteHeader(http.StatusCreated)
					_ = json.NewEncoder(w).Encode(map[string]any{"wallet_id": walletID, "owner_type": "PERSONAL", "owner_id": userID, "status": "ACTIVE", "version": 1})
				case "/internal/v1/wallets/" + walletID.String() + "/balance":
					w.WriteHeader(test.billingStatus)
					_ = json.NewEncoder(w).Encode(map[string]any{"wallet_id": walletID, "owner_type": "PERSONAL", "owner_id": userID, "status": "ACTIVE", "version": 1, "available_microcredits": 12345})
				default:
					t.Errorf("unexpected Billing request %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer billing.Close()
			contexts, err := NewHTTPContextResolver(account.URL)
			if err != nil {
				t.Fatal(err)
			}
			wallets, err := NewHTTPWalletResolver(billing.URL, "fixture-service-token")
			if err != nil {
				t.Fatal(err)
			}
			balances, err := NewHTTPBillingClient(billing.URL, "fixture-service-token")
			if err != nil {
				t.Fatal(err)
			}
			authorizer, err := authorization.NewClient(account.URL, "fixture-service-token")
			if err != nil {
				t.Fatal(err)
			}
			service := NewService(contexts, wallets, balances).WithAuthorizer(authorizer)
			handler := service.Handler(func(ctx context.Context, token string) (identity.Principal, error) {
				if token != "fixture-user-token" {
					return identity.Principal{}, fmt.Errorf("unexpected user token")
				}
				return identity.Principal{Issuer: "fixture", Subject: "subject"}, nil
			})
			request := httptest.NewRequest(http.MethodGet, "/v1/me/balance", nil)
			request.Header.Set("Authorization", "Bearer fixture-user-token")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus || billingHits.Load() != test.wantBillingHits {
				t.Fatalf("status %d, Billing hits %d; want %d / %d; body %s", response.Code, billingHits.Load(), test.wantStatus, test.wantBillingHits, response.Body.String())
			}
			if response.Code == http.StatusOK && (!strings.Contains(response.Body.String(), walletID.String()) || !strings.Contains(response.Body.String(), "12345")) {
				t.Fatal("the balance response did not preserve actual Billing facts")
			}
			for _, path := range []string{"/v1/models", "/v1/me/recharge-orders", "/v1/organizations", "/v1/me/usage-requests", "/admin/v1/token-accounts"} {
				absent := httptest.NewRecorder()
				handler.ServeHTTP(absent, httptest.NewRequest(http.MethodGet, path, nil))
				if absent.Code != http.StatusNotFound {
					t.Errorf("future route %s is reachable: %d", path, absent.Code)
				}
			}
		})
	}
}
