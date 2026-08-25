package token

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/humandelegation"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func TestOrganizationQuotaClientReplacesBodyActorWithSignedDelegation(t *testing.T) {
	publicKey, privateKey, _ := ed25519.GenerateKey(nil)
	signer, _ := humandelegation.NewSigner("account-test", privateKey)
	verifier, _ := humandelegation.NewVerifier(map[string]ed25519.PublicKey{"account-test": publicKey})
	humanID, organizationID := uuid.New(), uuid.New()
	accountServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Audience             string     `json:"audience"`
			Permission           string     `json:"permission"`
			TargetOrganizationID *uuid.UUID `json:"target_organization_id"`
			TargetResourceType   string     `json:"target_resource_type"`
			TargetResourceID     string     `json:"target_resource_id"`
			HTTPMethod           string     `json:"http_method"`
			HTTPPath             string     `json:"http_path"`
			RequestSHA256        string     `json:"request_sha256"`
		}
		if r.URL.Path != "/internal/v1/authorization/human-delegations" || json.NewDecoder(r.Body).Decode(&request) != nil {
			http.Error(w, "invalid", http.StatusBadRequest)
			return
		}
		token, err := signer.Sign(humandelegation.Claims{Audience: request.Audience, HumanUserID: humanID, Permission: request.Permission, AuthorizationVersion: 8, OrganizationID: request.TargetOrganizationID, ResourceType: request.TargetResourceType, ResourceID: request.TargetResourceID, HTTPMethod: request.HTTPMethod, HTTPPath: request.HTTPPath, RequestSHA256: request.RequestSHA256})
		if err != nil {
			http.Error(w, "invalid", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"delegation": token})
	}))
	defer accountServer.Close()
	authorizer, _ := authorization.NewClient(accountServer.URL, "service-secret")

	billingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "actor_user_id") || strings.Contains(string(body), "created_by") {
			http.Error(w, "body actor forbidden", http.StatusBadRequest)
			return
		}
		_, err := verifier.Verify(r.Header.Get("X-Pinvou-Human-Delegation"), humandelegation.Expectation{Audience: "billing-service", Permission: "org.quota.allocate", OrganizationID: &organizationID, ResourceType: "organization", ResourceID: organizationID.String(), HTTPMethod: http.MethodPost, HTTPPath: r.URL.Path, RequestSHA256: humandelegation.RequestDigest(body)})
		if err != nil {
			http.Error(w, "invalid delegation", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer billingServer.Close()
	client, _ := NewHTTPBillingClient(billingServer.URL, "service-secret")
	client.WithHumanDelegations(authorizer)

	router := gin.New()
	router.Use(identity.GinMiddleware(func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "https://identity.example", Subject: "human-subject"}, nil
	}))
	router.POST("/invoke", func(c *gin.Context) {
		_, err := client.AllocateOrganizationQuotaBatch(c.Request.Context(), organizationID, uuid.New(), json.RawMessage(`[{"member_user_id":"`+uuid.NewString()+`"}]`), "1234567890abcdef")
		if err != nil {
			c.String(http.StatusBadGateway, err.Error())
			return
		}
		c.Status(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodPost, "/invoke", nil)
	request.Header.Set("Authorization", "Bearer human-token")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("delegated Billing call failed: %d %s", response.Code, response.Body.String())
	}
}
