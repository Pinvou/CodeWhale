package token

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

type fixedBillingContextResolver struct{ resolved AuthorizationContext }

func (r fixedBillingContextResolver) Resolve(context.Context, string) (AuthorizationContext, error) {
	return r.resolved, nil
}

type recordingWalletResolver struct {
	ownerType string
	ownerID   uuid.UUID
	walletID  uuid.UUID
}

func (r *recordingWalletResolver) Ensure(_ context.Context, ownerType string, ownerID uuid.UUID) (uuid.UUID, error) {
	r.ownerType, r.ownerID = ownerType, ownerID
	return r.walletID, nil
}

type authorizationDecisionRecorder struct {
	mu             sync.Mutex
	allowed        bool
	userID         uuid.UUID
	permissions    []string
	organizationID *uuid.UUID
}

func (r *authorizationDecisionRecorder) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	var body struct {
		Permission           string     `json:"permission"`
		TargetOrganizationID *uuid.UUID `json:"target_organization_id"`
	}
	if request.URL.Path != "/internal/v1/authorization/check" || json.NewDecoder(request.Body).Decode(&body) != nil {
		http.Error(w, "invalid authorization request", http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	r.permissions = append(r.permissions, body.Permission)
	r.organizationID = body.TargetOrganizationID
	r.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Authorization-Version", "17")
	_ = json.NewEncoder(w).Encode(map[string]any{"allowed": r.allowed, "user_id": r.userID})
}

func TestOrganizationBalanceUsesExplicitTargetWithoutActiveContextSwitch(t *testing.T) {
	userID, organizationID, walletID := uuid.New(), uuid.New(), uuid.New()
	authorizationRecorder := &authorizationDecisionRecorder{allowed: true, userID: userID}
	authorizationServer := httptest.NewServer(authorizationRecorder)
	defer authorizationServer.Close()
	authorizer, err := authorization.NewClient(authorizationServer.URL, "service-token")
	if err != nil {
		t.Fatal(err)
	}
	billingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/internal/v1/wallets/"+walletID.String()+"/balance" {
			t.Fatalf("unexpected Billing path %q", request.URL.Path)
		}
		_, _ = w.Write([]byte(`{"available_microcredits":42}`))
	}))
	defer billingServer.Close()
	billingClient, err := NewHTTPBillingClient(billingServer.URL, "service-token")
	if err != nil {
		t.Fatal(err)
	}
	wallets := &recordingWalletResolver{walletID: walletID}
	service := NewService(nil, fixedBillingContextResolver{resolved: AuthorizationContext{UserID: userID, ContextType: "PERSONAL"}}, wallets, nil, ProviderURLPolicy{}, billingClient, nil, 0).WithAuthorizer(authorizer)
	request := httptest.NewRequest(http.MethodGet, "/v1/organizations/"+organizationID.String()+"/balance", nil)
	request.Header.Set("Authorization", "Bearer oidc-token")
	response := httptest.NewRecorder()
	service.Handler("service-token", successfulBillingVerify).ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != `{"available_microcredits":42}` {
		t.Fatalf("unexpected response: %d %s", response.Code, response.Body.String())
	}
	if wallets.ownerType != "ORGANIZATION" || wallets.ownerID != organizationID {
		t.Fatalf("wallet scope = %s/%s, want explicit organization", wallets.ownerType, wallets.ownerID)
	}
	if len(authorizationRecorder.permissions) != 1 || authorizationRecorder.permissions[0] != "org.wallet.read" || authorizationRecorder.organizationID == nil || *authorizationRecorder.organizationID != organizationID {
		t.Fatalf("unexpected authorization decision: %#v / %v", authorizationRecorder.permissions, authorizationRecorder.organizationID)
	}
}

func TestPersonalBalanceCannotBorrowPermissionFromOrganizationContext(t *testing.T) {
	userID, organizationID := uuid.New(), uuid.New()
	authorizationRecorder := &authorizationDecisionRecorder{allowed: true, userID: userID}
	authorizationServer := httptest.NewServer(authorizationRecorder)
	defer authorizationServer.Close()
	authorizer, err := authorization.NewClient(authorizationServer.URL, "service-token")
	if err != nil {
		t.Fatal(err)
	}
	billingCalled := false
	billingServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { billingCalled = true }))
	defer billingServer.Close()
	billingClient, err := NewHTTPBillingClient(billingServer.URL, "service-token")
	if err != nil {
		t.Fatal(err)
	}
	wallets := &recordingWalletResolver{walletID: uuid.New()}
	service := NewService(nil, fixedBillingContextResolver{resolved: AuthorizationContext{UserID: userID, ContextType: "ORGANIZATION", OrganizationID: &organizationID}}, wallets, nil, ProviderURLPolicy{}, billingClient, nil, 0).WithAuthorizer(authorizer)
	request := httptest.NewRequest(http.MethodGet, "/v1/me/balance", nil)
	request.Header.Set("Authorization", "Bearer oidc-token")
	response := httptest.NewRecorder()
	service.Handler("service-token", successfulBillingVerify).ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || billingCalled {
		t.Fatalf("organization context accessed personal balance: status=%d billing_called=%v", response.Code, billingCalled)
	}
	if len(authorizationRecorder.permissions) != 1 || authorizationRecorder.permissions[0] != "personal.billing.read" || authorizationRecorder.organizationID != nil {
		t.Fatalf("unexpected personal authorization decision: %#v / %v", authorizationRecorder.permissions, authorizationRecorder.organizationID)
	}
}

func successfulBillingVerify(context.Context, string) (identity.Principal, error) {
	return identity.Principal{Issuer: "https://issuer.example", Subject: "subject"}, nil
}
