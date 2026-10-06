package billing

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
)

// A nil database is deliberate: malformed bodies must be rejected by the
// production HTTP handlers before invoking a repository or payment operation.
type financialBodyGuardRepository struct{ *PostgresRepository }

func TestFinancialMalformedBodiesReturnStatic400(t *testing.T) {
	service := NewService(financialBodyGuardRepository{}).WithPayments(&PaymentCoordinator{}).WithAuthorizer(billingTestAuthorizer(t))
	server := httptest.NewServer(service.Handler("controlled-service-token", func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "https://fixture-authority.invalid", Subject: "fixture-platform-operator"}, nil
	}))
	defer server.Close()
	id := "11111111-1111-4111-8111-111111111111"
	routes := map[string]string{
		"schedule":               "/admin/v1/late-payment-reviews/" + id + "/schedule",
		"provider-blind":         "/admin/v1/late-payment-reviews/" + id + "/provider-blind",
		"import":                 "/admin/v1/reconciliation-imports",
		"run":                    "/admin/v1/reconciliation-runs",
		"reconciliation-command": "/admin/v1/reconciliation-cases/" + id + "/commands",
		"anomaly-resolution":     "/admin/v1/payment-anomalies/" + id + "/resolve",
		"safe-release":           "/admin/v1/usage-holds/" + id + "/release",
		"simple-pricing":         "/admin/v1/pricing-versions",
	}
	bodies := map[string]string{"malformed": "{", "unknown": `{"unexpected":true}`, "trailing": `{} garbage`, "two-values": `{} {}`, "oversize": `{"unexpected":"` + strings.Repeat("x", 1<<20) + `"}`}
	for name, path := range routes {
		t.Run(name, func(t *testing.T) {
			for kind, body := range bodies {
				t.Run(kind, func(t *testing.T) {
					request, err := http.NewRequest(http.MethodPost, server.URL+path, strings.NewReader(body))
					if err != nil {
						t.Fatal(err)
					}
					request.Header.Set("Authorization", "Bearer controlled-fixture-user")
					request.Header.Set("Content-Type", "application/json")
					request.Header.Set("Idempotency-Key", "controlled-financial-key-000001")
					response, err := server.Client().Do(request)
					if err != nil {
						t.Fatal(err)
					}
					defer response.Body.Close()
					payload, err := io.ReadAll(io.LimitReader(response.Body, 4096))
					if err != nil {
						t.Fatal(err)
					}
					var result struct {
						Code    string `json:"code"`
						Message string `json:"message"`
					}
					if response.StatusCode != 400 || json.Unmarshal(payload, &result) != nil || result.Code != "INVALID_REQUEST" {
						t.Fatalf("status=%d code=%q", response.StatusCode, result.Code)
					}
					if strings.Contains(result.Message, "unexpected") || strings.Contains(result.Message, "garbage") {
						t.Fatal("request contents leaked")
					}
				})
			}
		})
	}
}
