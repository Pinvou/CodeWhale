package token

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"
)

type readinessCatalogFake struct{ model CatalogModel }

func (f readinessCatalogFake) ListCatalog(context.Context) (int64, []CatalogModel, error) {
	return 7, []CatalogModel{f.model}, nil
}

type readinessRoutesFake struct{}

func (readinessRoutesFake) ValidateModelReady(context.Context, uuid.UUID, string) error { return nil }

type readinessBillingFake struct{ calls int }

func (f *readinessBillingFake) Price(_ context.Context, model string) (PriceVersion, error) {
	f.calls++
	formula, _ := json.Marshal(map[string]any{"evaluator_version": "SAFE_PARAMETER_MAP_V1", "expression": map[string]string{"INPUT_TOKEN": "estimated_input_tokens", "CACHED_INPUT_TOKEN": "cached_input_tokens", "OUTPUT_TOKEN": "max_output_tokens"}})
	units := []ResolvedUnitPrice{
		{UnitCode: "INPUT_TOKEN", UnitDefinitionID: uuid.New(), UnitVersion: 1, MinimumPrecision: 1, PriceDenominator: 1, ResolvedPrice: 1},
		{UnitCode: "CACHED_INPUT_TOKEN", UnitDefinitionID: uuid.New(), UnitVersion: 1, MinimumPrecision: 1, PriceDenominator: 1, ResolvedPrice: 1},
		{UnitCode: "OUTPUT_TOKEN", UnitDefinitionID: uuid.New(), UnitVersion: 1, MinimumPrecision: 1, PriceDenominator: 1, ResolvedPrice: 1},
	}
	return PriceVersion{ID: uuid.New(), Model: model, ProtocolCode: "CHAT", FormulaVersionID: uuid.New(), FormulaSnapshot: formula, CreditUnit: "MICROCREDIT", Units: units}, nil
}

type readinessRecorderFake struct{ recorded int }

func (f *readinessRecorderFake) RecordClientReadinessAttestation(context.Context, ClientReadinessAttestation, []byte) error {
	f.recorded++
	return nil
}

func TestClientReadinessIsNonBillingAndIssuesBoundEd25519Attestation(t *testing.T) {
	const workloadAudience = "pinvou-account-platform-internal"
	workloadKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	workloadJWKS := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &workloadKey.PublicKey, KeyID: "readiness-fixture", Algorithm: "RS256", Use: "sig"}}})
	}))
	t.Cleanup(workloadJWKS.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: workloadJWKS.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	workloadIssuer := workloadJWKS.URL + "/workload"
	t.Setenv("APP_ENV", "production")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", workloadIssuer)
	t.Setenv("WORKLOAD_IDENTITY_JWKS_URL", workloadJWKS.URL)
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", ca)
	t.Setenv("WORKLOAD_IDENTITY_AUDIENCE", workloadAudience)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: workloadKey}, (&jose.SignerOptions{}).WithHeader("kid", "readiness-fixture"))
	if err != nil {
		t.Fatal(err)
	}
	sign := func(subject string) string {
		t.Helper()
		now := time.Now()
		token, err := jwt.Signed(signer).Claims(jwt.Claims{
			Issuer: workloadIssuer, Subject: subject, Audience: jwt.Audience{workloadAudience},
			IssuedAt: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(now.Add(time.Minute)),
		}).Serialize()
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	productionRules, err := os.ReadFile("../../deploy/production/workload-token-rules.json.template")
	if err != nil {
		t.Fatal(err)
	}
	const accountSubject = "pinvou:service:account-service"
	const billingSubject = "pinvou:service:billing-service"
	rawRules := strings.NewReplacer("__ACCOUNT_WORKLOAD_SUBJECT__", accountSubject,
		"__BILLING_WORKLOAD_SUBJECT__", billingSubject).Replace(string(productionRules))
	accountJWT, billingJWT := sign(accountSubject), sign(billingSubject)

	serviceToken := "service-token"
	var issuer *httptest.Server
	issuer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			t.Fatalf("unexpected OIDC path %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": issuer.URL, "jwks_uri": issuer.URL + "/jwks"})
	}))
	defer issuer.Close()
	account := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+serviceToken {
			t.Fatal("account readiness omitted service identity")
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"service": "account", "service_version": "0.8.0", "contract_version": "account-client-v1"})
	}))
	defer account.Close()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	billing, recorder := &readinessBillingFake{}, &readinessRecorderFake{}
	modelID := uuid.New()
	readiness, err := NewClientReadinessService(ClientReadinessConfig{Environment: "test", ServiceVersion: "0.8.0", ContractVersion: "client-readiness-v1", MinimumClient: "0.8.0", MaximumClient: "0.8.0", AccountURL: account.URL, OIDCIssuer: issuer.URL, ServiceToken: serviceToken, SigningKeyID: "test-key", SigningKey: private, AttestationTTL: 5 * time.Minute}, readinessCatalogFake{model: CatalogModel{ModelID: modelID, CatalogVersion: 7, LifecycleStatus: "PUBLISHED", SourceAvailable: true, ProtocolCode: "CHAT"}}, readinessRoutesFake{}, billing, recorder)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{personalExecutionEnabled: true, clientReadiness: readiness}
	t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", rawRules)
	handler := service.Handler(nil)
	request := func(handler http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		handler.ServeHTTP(response, r)
		return response
	}
	const readPath = "/internal/v1/client-readiness?protocol=CHAT"
	const attestPath = "/internal/v1/client-readiness/attestations"
	const artifact = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const attestBody = `{"artifact_sha256":"` + artifact + `","client_version":"0.8.0","environment":"test","protocol_code":"CHAT"}`
	for _, operation := range []struct{ method, path, body, permission string }{
		{http.MethodGet, readPath, "", "internal.token.client-readiness.read"},
		{http.MethodPost, attestPath, attestBody, "internal.token.client-readiness.attest"},
	} {
		t.Run(operation.permission, func(t *testing.T) {
			if response := request(handler, operation.method, operation.path, operation.body, billingJWT); response.Code != http.StatusForbidden {
				t.Fatalf("billing subject received %d, want 403", response.Code)
			}
			var rules []struct {
				Subject     string   `json:"subject"`
				Service     string   `json:"service"`
				Permissions []string `json:"permissions"`
			}
			if err := json.Unmarshal([]byte(rawRules), &rules); err != nil {
				t.Fatal(err)
			}
			for i := range rules {
				if rules[i].Subject == accountSubject {
					rules[i].Permissions = slices.DeleteFunc(rules[i].Permissions, func(permission string) bool {
						return permission == operation.permission
					})
				}
			}
			changed, err := json.Marshal(rules)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", string(changed))
			if response := request(service.Handler(nil), operation.method, operation.path, operation.body, accountJWT); response.Code != http.StatusForbidden {
				t.Fatalf("Account subject without exact permission received %d, want 403", response.Code)
			}
		})
	}
	response := request(handler, http.MethodGet, readPath, "", accountJWT)
	var result ClientReadinessResult
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil {
		t.Fatalf("readiness query received %d %s", response.Code, response.Body.String())
	}
	if !result.Ready || billing.calls != 1 || result.CatalogVersion != 7 {
		t.Fatalf("unexpected readiness result %#v, billing calls %d", result, billing.calls)
	}
	response = request(handler, http.MethodPost, attestPath, attestBody, accountJWT)
	var attestation ClientReadinessAttestation
	if response.Code != http.StatusCreated || json.Unmarshal(response.Body.Bytes(), &attestation) != nil {
		t.Fatalf("readiness attestation received %d %s", response.Code, response.Body.String())
	}
	payload, _ := json.Marshal(attestation.Claims)
	signature, err := base64.RawURLEncoding.DecodeString(attestation.Signature)
	if err != nil || !ed25519.Verify(public, payload, signature) {
		t.Fatal("attestation signature is invalid")
	}
	if attestation.Claims.ArtifactSHA256 != artifact || attestation.Claims.Environment != "test" || recorder.recorded != 1 {
		t.Fatalf("attestation is not correctly bound: %#v", attestation)
	}
}

func TestClientReadinessRejectsUnboundOrUnsignedAttestations(t *testing.T) {
	key, err := DecodeClientReadinessSigningKey(base64.RawStdEncoding.EncodeToString(make([]byte, ed25519.SeedSize)))
	if err != nil || len(key) != ed25519.PrivateKeySize {
		t.Fatal("seed did not decode")
	}
	if _, err := DecodeClientReadinessSigningKey("not-base64"); err == nil {
		t.Fatal("invalid signing key was accepted")
	}
}
