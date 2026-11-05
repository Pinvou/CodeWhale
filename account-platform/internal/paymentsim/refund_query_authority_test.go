package paymentsim

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

type refundQueryAuthorityRepository struct {
	Repository
	queries int
}

func (r *refundQueryAuthorityRepository) QueryRefund(_ context.Context, order, refund string) (Refund, error) {
	if order != "original-order" || refund != "original-refund" {
		return Refund{}, ErrNotFound
	}
	r.queries++
	return Refund{MerchantRefundNo: refund, ChannelRefundID: "stored-original-ref", AmountFen: 100, Status: "SUCCESS"}, nil
}

func TestRefundQueryRequiresExactReadWorkloadScope(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "controlled", Algorithm: "RS256", Use: "sig"}}})
	}))
	defer jwks.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: jwks.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	issuer := jwks.URL + "/workload"
	t.Setenv("APP_ENV", "production")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", issuer)
	t.Setenv("WORKLOAD_IDENTITY_JWKS_URL", jwks.URL)
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", ca)
	t.Setenv("WORKLOAD_IDENTITY_AUDIENCE", "payment-simulator")
	t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", `[{"subject":"billing-query","service":"billing-service","permissions":["internal.payment-simulator.refund.read"]},{"subject":"write-only","service":"billing-service","permissions":["internal.payment-simulator.refund.create"]}]`)
	repository := &refundQueryAuthorityRepository{}
	handler := NewService(repository).Handler("")
	for _, tc := range []struct {
		subject string
		status  int
	}{{"", 401}, {"write-only", 403}, {"unregistered", 403}, {"billing-query", 200}} {
		t.Run(tc.subject, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/internal/v1/orders/original-order/refunds/original-refund", nil)
			if tc.subject != "" {
				signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "controlled"))
				if err != nil {
					t.Fatal(err)
				}
				claims, _ := json.Marshal(map[string]any{"iss": issuer, "aud": "payment-simulator", "sub": tc.subject, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix()})
				signed, err := signer.Sign(claims)
				if err != nil {
					t.Fatal(err)
				}
				raw, err := signed.CompactSerialize()
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Authorization", "Bearer "+raw)
			}
			before := repository.queries
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.status {
				t.Fatal("refund query authority", response.Code, tc.status)
			}
			if tc.status == 200 {
				if repository.queries != before+1 {
					t.Fatal("authorized original query did not reach its exact keys")
				}
			} else if repository.queries != before {
				t.Fatal("denied workload reached refund repository")
			}
		})
	}
}
