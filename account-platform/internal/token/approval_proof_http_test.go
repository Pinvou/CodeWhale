package token

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	accountdomain "github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"
)

func tokenProofWorkloadIssuer(t *testing.T) (string, func(string) string) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "isolated proof CA"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: ca.NotBefore, NotAfter: ca.NotAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	issuer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/certs" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "proof", Algorithm: "RS256", Use: "sig"}}})
	}))
	issuer.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER, caDER}, PrivateKey: leafKey}}}
	issuer.StartTLS()
	t.Cleanup(issuer.Close)
	caFile := filepath.Join(t.TempDir(), "workload-ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("APP_ENV", "production")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", issuer.URL+"/realms/workload")
	t.Setenv("WORKLOAD_IDENTITY_JWKS_URL", issuer.URL+"/certs")
	t.Setenv("WORKLOAD_IDENTITY_AUDIENCE", "token-proof-api")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", caFile)
	t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", `[{"subject":"account-subject","service":"account-service","permissions":["internal.token.approval-proof.read"]},{"subject":"portal-subject","service":"token-credential-portal","permissions":["internal.token.approval-proof.read"]}]`)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "proof"))
	if err != nil {
		t.Fatal(err)
	}
	return issuer.URL, func(subject string) string {
		claims, _ := json.Marshal(map[string]any{"iss": issuer.URL + "/realms/workload", "aud": "token-proof-api", "sub": subject,
			"iat": time.Now().Unix(), "exp": time.Now().Add(5 * time.Minute).Unix()})
		value, err := signer.Sign(claims)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := value.CompactSerialize()
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
}

func TestAccountHTTPTokenProofUsesSignedExactWorkloadIdentity(t *testing.T) {
	_, sign := tokenProofWorkloadIssuer(t)
	accountID, credentialID := uuid.New(), uuid.New()
	accountVersion, credentialVersion := int64(4), 2
	fingerprint := strings.Repeat("a", 64)
	evidence := ImpactLocalEvidence{ResourceType: "PROVIDER_CREDENTIAL", ResourceID: credentialID.String(), CurrentStatus: "ACTIVE",
		CurrentVersion: ImpactResourceVersion{AccountVersion: &accountVersion, CredentialVersion: &credentialVersion},
		ObservedAt:     time.Now().UTC(), CredentialFingerprint: fingerprint, RouteSetFingerprint: fingerprint,
		ProvisioningSetFingerprint: fingerprint, InflightSetFingerprint: fingerprint}
	service := NewService(nil, nil, nil, nil, ProviderURLPolicy{}, nil, nil, 0).
		WithImpactPrechecks(true, stubImpactRepository{evidence: evidence}, nil).
		WithTokenCommandGovernanceActivation(testTokenGovernanceActivationReader{activation: TokenCommandGovernanceActivation{State: "ACTIVE", Epoch: 7}})
	server := httptest.NewServer(service.Handler("legacy-static", nil))
	defer server.Close()
	command := json.RawMessage(`{"token_account_id":"` + accountID.String() + `","expected_account_version":4,"credential_id":"` + credentialID.String() + `","credential_version":2}`)
	projection, err := service.approvalCommandPrecheck(context.Background(), ApprovalSnapshotRequest{CommandType: "TOKEN_CREDENTIAL_REVOKE", TargetResourceType: "PROVIDER_CREDENTIAL", TargetResourceID: credentialID.String(), NormalizedCommand: command})
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{CommandType: "TOKEN_CREDENTIAL_REVOKE", PolicyVersion: 1, SnapshotSchemaVersion: 1,
		TargetResourceType: "PROVIDER_CREDENTIAL", TargetResourceID: credentialID.String(), NormalizedCommand: command,
		CriticalResourceVersions: projection.CriticalResourceVersions, FinancialSnapshot: projection.FinancialSnapshot})
	if err != nil {
		t.Fatal(err)
	}
	source, err := accountdomain.NewHTTPApprovalProofSource(server.URL, server.URL, sign("account-subject"))
	if err != nil {
		t.Fatal(err)
	}
	requestID := uuid.New()
	result, err := source.Proof(context.Background(), requestID, artifacts)
	if err != nil || result.Digest == "" || result.Response.Verdict != "ALLOW" || result.Response.ApprovalRequestID != requestID {
		t.Fatalf("signed Account proof=%+v err=%v", result, err)
	}
	accountVersion = 5
	service.WithImpactPrechecks(true, stubImpactRepository{evidence: evidence}, nil)
	denied, err := source.Proof(context.Background(), requestID, artifacts)
	if !errors.Is(err, accountdomain.ErrApprovalProofRejected) || denied.Response.ReasonCode != "RESOURCE_VERSION_CHANGED" || denied.Response.DomainRevision == result.Response.DomainRevision {
		t.Fatalf("signed Account denial=%+v err=%v", denied, err)
	}
	body, err := approvalencoding.EncodeProofRequest(requestID, artifacts)
	if err != nil {
		t.Fatal(err)
	}
	for name, scenario := range map[string]struct {
		bearer string
		status int
	}{
		"portal even with permission": {sign("portal-subject"), http.StatusForbidden},
		"static shared token":         {"legacy-static", http.StatusUnauthorized},
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/internal/v1/approval-command-proofs", strings.NewReader(string(body)))
			request.Header.Set("Authorization", "Bearer "+scenario.bearer)
			response := httptest.NewRecorder()
			service.Handler("legacy-static", nil).ServeHTTP(response, request)
			if response.Code != scenario.status {
				t.Fatalf("wrong workload crossed proof route status=%d", response.Code)
			}
		})
	}
}
