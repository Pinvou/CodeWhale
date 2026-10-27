package serviceauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
)

// Verify the shipped production rules at the real authenticated-identity and
// Require boundary. The issuer verifier is controlled; this is not an IdP test.
func TestProductionAccountDelegationRemainsCallerBound(t *testing.T) {
	body, err := os.ReadFile("../../../deploy/production/workload-account-rules.json.template")
	if err != nil {
		t.Fatal(err)
	}
	var rules []rule
	if err := json.Unmarshal(body, &rules); err != nil {
		t.Fatal(err)
	}
	for i := range rules {
		rules[i].Subject = "system:serviceaccount:public-fixture:" + rules[i].Service
	}
	for _, tc := range []struct {
		service, permission string
		want                int
	}{
		{"billing-service", "internal.authorization.delegate", http.StatusForbidden},
		{"token-gateway", "internal.authorization.delegate", http.StatusNoContent},
		{"billing-service", "internal.authorization.check", http.StatusNoContent},
		{"billing-service", "internal.approval-execution.begin", http.StatusNoContent},
		{"token-credential-portal", "internal.authorization.delegate", http.StatusForbidden},
		{"account-approval-recovery", "internal.authorization.delegate", http.StatusForbidden},
	} {
		t.Run(tc.service+"/"+tc.permission, func(t *testing.T) {
			auth := authenticator{rules: rules, verify: func(context.Context, string) (string, error) {
				return "system:serviceaccount:public-fixture:" + tc.service, nil
			}}
			reached := false
			router := gin.New()
			router.Use(func(c *gin.Context) {
				identity, status := auth.authenticate(c.Request)
				if status != http.StatusOK {
					c.AbortWithStatus(status)
					return
				}
				c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), identityContextKey{}, identity))
				c.Next()
			})
			router.POST("/checked-operation", Require(tc.permission), func(c *gin.Context) {
				reached = true
				c.Status(http.StatusNoContent)
			})
			request := httptest.NewRequest(http.MethodPost, "/checked-operation", nil)
			request.Header.Set("Authorization", "Bearer public-fixture-only")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != tc.want || reached != (tc.want == http.StatusNoContent) {
				t.Fatalf("permission boundary: status=%d reached=%v", response.Code, reached)
			}
		})
	}
}
