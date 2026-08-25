package billing

import (
	"crypto/ed25519"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/humandelegation"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func TestHumanDelegationMiddlewareBindsWorkloadActorAndExactRequest(t *testing.T) {
	publicKey, privateKey, _ := ed25519.GenerateKey(nil)
	signer, _ := humandelegation.NewSigner("account-test", privateKey)
	verifier, _ := humandelegation.NewVerifier(map[string]ed25519.PublicKey{"account-test": publicKey})
	service := NewService(nil).WithHumanDelegations(verifier)
	organizationID, humanID := uuid.New(), uuid.New()
	path := "/internal/v1/organizations/" + organizationID.String() + "/quota-grants:batch"
	body := []byte(`{"items":[{"amount_microcredits":200}]}`)
	token, err := signer.Sign(humandelegation.Claims{Audience: "billing-service", HumanUserID: humanID, Permission: "org.quota.allocate", AuthorizationVersion: 4, OrganizationID: &organizationID, ResourceType: "organization", ResourceID: organizationID.String(), HTTPMethod: http.MethodPost, HTTPPath: path, RequestSHA256: humandelegation.RequestDigest(body)})
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.Use(serviceauth.GinMiddleware("service-secret"))
	router.POST("/internal/v1/organizations/:organization_id/quota-grants:batch", serviceauth.Require("internal.billing.quota.allocate"), service.requireHumanDelegation("org.quota.allocate", "organization", "organization_id"), func(c *gin.Context) {
		workload, workloadOK := serviceauth.FromContext(c.Request.Context())
		delegation, delegationOK := humandelegation.FromContext(c.Request.Context())
		restored, _ := io.ReadAll(c.Request.Body)
		if !workloadOK || workload.Permission != "internal.billing.quota.allocate" || !delegationOK || delegation.HumanUserID != humanID || string(restored) != string(body) {
			c.Status(http.StatusInternalServerError)
			return
		}
		c.Status(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer service-secret")
	request.Header.Set(humanDelegationHeader, token)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("valid delegation rejected: %d %s", response.Code, response.Body.String())
	}

	tampered := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"items":[]}`))
	tampered.Header.Set("Authorization", "Bearer service-secret")
	tampered.Header.Set(humanDelegationHeader, token)
	tamperedResponse := httptest.NewRecorder()
	router.ServeHTTP(tamperedResponse, tampered)
	if tamperedResponse.Code != http.StatusForbidden {
		t.Fatalf("body substitution must fail: %d", tamperedResponse.Code)
	}
}
