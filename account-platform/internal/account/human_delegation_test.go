package account

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/humandelegation"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

type delegationRepository struct {
	*fakeRepository
	userID uuid.UUID
}

func (r *delegationRepository) Authorize(_ context.Context, _ identity.Principal, permission string, organizationID, _ *uuid.UUID) (AuthorizationDecision, error) {
	return AuthorizationDecision{Allowed: !r.deny, UserID: r.userID, Permission: permission, OrganizationID: organizationID, ContextVersion: 3, AuthorizationVersion: 9}, nil
}

func TestCreateHumanDelegationUsesAuthoritativeUserAndBindings(t *testing.T) {
	publicKey, privateKey, _ := ed25519.GenerateKey(nil)
	signer, _ := humandelegation.NewSigner("account-test", privateKey)
	userID, organizationID := uuid.New(), uuid.New()
	repository := &delegationRepository{fakeRepository: &fakeRepository{}, userID: userID}
	handler := NewService(repository, &fakeWalletProvisioner{}).WithServiceToken("service-secret").WithHumanDelegations(signer).Handler(nil)
	body := `{"issuer":"https://identity.example","subject":"human-subject","audience":"billing-service","permission":"org.quota.allocate","target_organization_id":"` + organizationID.String() + `","target_resource_type":"member-quota","target_resource_id":"grant-1","http_method":"POST","http_path":"/internal/v1/member-quota-grants","request_sha256":"` + humandelegation.RequestDigest([]byte(`{"amount":200}`)) + `"}`
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/authorization/human-delegations", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer service-secret")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Authorization-Version") != "9" {
		t.Fatalf("unexpected response: %d %s", response.Code, response.Body.String())
	}
	var result HumanDelegationResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	verifier, _ := humandelegation.NewVerifier(map[string]ed25519.PublicKey{"account-test": publicKey})
	claims, err := verifier.Verify(result.Delegation, humandelegation.Expectation{Audience: "billing-service", Permission: "org.quota.allocate", OrganizationID: &organizationID, ResourceType: "member-quota", ResourceID: "grant-1", HTTPMethod: http.MethodPost, HTTPPath: "/internal/v1/member-quota-grants", RequestSHA256: humandelegation.RequestDigest([]byte(`{"amount":200}`))})
	if err != nil || claims.HumanUserID != userID || claims.AuthorizationVersion != 9 {
		t.Fatalf("unexpected claims: %#v, %v", claims, err)
	}
}

func TestCreateHumanDelegationFailsClosed(t *testing.T) {
	repository := &delegationRepository{fakeRepository: &fakeRepository{deny: true}, userID: uuid.New()}
	handler := NewService(repository, &fakeWalletProvisioner{}).WithServiceToken("service-secret").Handler(nil)
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/authorization/human-delegations", strings.NewReader(`{"issuer":"issuer","subject":"subject","audience":"billing-service","permission":"org.quota.allocate"}`))
	request.Header.Set("Authorization", "Bearer service-secret")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing signer must fail closed: %d %s", response.Code, response.Body.String())
	}
}
