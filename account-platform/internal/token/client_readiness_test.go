package token

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
	result := readiness.Check(context.Background(), "CHAT")
	if !result.Ready || billing.calls != 1 || result.CatalogVersion != 7 {
		t.Fatalf("unexpected readiness result %#v, billing calls %d", result, billing.calls)
	}
	artifact := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	attestation, err := readiness.Issue(context.Background(), artifact, "0.8.0", "test", "CHAT")
	if err != nil {
		t.Fatal(err)
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
