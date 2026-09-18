package billing

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

type approvalActivationStub struct {
	Repository
	snapshot ApprovalCommandActivationSnapshot
	err      error
	calls    atomic.Int64
}

func (s *approvalActivationStub) ApprovalCommandActivations(context.Context) (ApprovalCommandActivationSnapshot, error) {
	s.calls.Add(1)
	return s.snapshot, s.err
}

func TestApprovalCommandActivationHTTP(t *testing.T) {
	now := time.Now().UTC()
	snapshot := validBillingApprovalActivationSnapshot(now)
	handler := NewService(&approvalActivationStub{snapshot: snapshot}).Handler("billing-activation-test-token")

	unauthorized := httptest.NewRequest(http.MethodGet, "/internal/v1/approval-command-activations", nil)
	unauthorizedResponse := httptest.NewRecorder()
	handler.ServeHTTP(unauthorizedResponse, unauthorized)
	if unauthorizedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("activation projection accepted browser request: %d", unauthorizedResponse.Code)
	}

	authorized := httptest.NewRequest(http.MethodGet, "/internal/v1/approval-command-activations", nil)
	authorized.Header.Set("Authorization", "Bearer billing-activation-test-token")
	authorizedResponse := httptest.NewRecorder()
	handler.ServeHTTP(authorizedResponse, authorized)
	var actual ApprovalCommandActivationSnapshot
	if authorizedResponse.Code != http.StatusOK || authorizedResponse.Header().Get("Cache-Control") != "no-store" || json.Unmarshal(authorizedResponse.Body.Bytes(), &actual) != nil || actual.SchemaVersion != 1 || len(actual.Activations) != 11 {
		t.Fatalf("unexpected activation response: status=%d cache=%q body=%s", authorizedResponse.Code, authorizedResponse.Header().Get("Cache-Control"), authorizedResponse.Body.String())
	}

	unavailableHandler := NewService(&approvalActivationStub{err: errors.New("database detail")}).Handler("billing-activation-test-token")
	unavailable := httptest.NewRequest(http.MethodGet, "/internal/v1/approval-command-activations", nil)
	unavailable.Header.Set("Authorization", "Bearer billing-activation-test-token")
	unavailableResponse := httptest.NewRecorder()
	unavailableHandler.ServeHTTP(unavailableResponse, unavailable)
	if unavailableResponse.Code != http.StatusServiceUnavailable || !containsJSONCode(unavailableResponse.Body.Bytes(), "BILLING_APPROVAL_ACTIVATION_UNAVAILABLE") {
		t.Fatalf("unclassified repository error should not be exposed: %d %s", unavailableResponse.Code, unavailableResponse.Body.String())
	}

	typedUnavailableHandler := NewService(&approvalActivationStub{err: ErrApprovalActivationUnavailable}).Handler("billing-activation-test-token")
	typedUnavailable := httptest.NewRequest(http.MethodGet, "/internal/v1/approval-command-activations", nil)
	typedUnavailable.Header.Set("Authorization", "Bearer billing-activation-test-token")
	typedUnavailableResponse := httptest.NewRecorder()
	typedUnavailableHandler.ServeHTTP(typedUnavailableResponse, typedUnavailable)
	if typedUnavailableResponse.Code != http.StatusServiceUnavailable || !json.Valid(typedUnavailableResponse.Body.Bytes()) || !containsJSONCode(typedUnavailableResponse.Body.Bytes(), "BILLING_APPROVAL_ACTIVATION_UNAVAILABLE") {
		t.Fatalf("typed activation failure lost stable response: %d %s", typedUnavailableResponse.Code, typedUnavailableResponse.Body.String())
	}

	missingRepository := NewService(nil).Handler("billing-activation-test-token")
	missing := httptest.NewRequest(http.MethodGet, "/internal/v1/approval-command-activations", nil)
	missing.Header.Set("Authorization", "Bearer billing-activation-test-token")
	missingResponse := httptest.NewRecorder()
	missingRepository.ServeHTTP(missingResponse, missing)
	if missingResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing production projection did not fail closed: %d %s", missingResponse.Code, missingResponse.Body.String())
	}

	for _, test := range []struct {
		name   string
		mutate func(*ApprovalCommandActivationSnapshot)
	}{
		{"partial", func(value *ApprovalCommandActivationSnapshot) { value.Activations = value.Activations[:10] }},
		{"wrong-order", func(value *ApprovalCommandActivationSnapshot) {
			value.Activations[0], value.Activations[1] = value.Activations[1], value.Activations[0]
		}},
		{"unknown-state", func(value *ApprovalCommandActivationSnapshot) { value.Activations[0].State = "UNKNOWN" }},
		{"policy-only-active", func(value *ApprovalCommandActivationSnapshot) { value.Activations[0].State = "ACTIVE" }},
	} {
		t.Run("invalid-repository-"+test.name, func(t *testing.T) {
			invalid := validBillingApprovalActivationSnapshot(now)
			test.mutate(&invalid)
			invalidHandler := NewService(&approvalActivationStub{snapshot: invalid}).Handler("billing-activation-test-token")
			request := httptest.NewRequest(http.MethodGet, "/internal/v1/approval-command-activations", nil)
			request.Header.Set("Authorization", "Bearer billing-activation-test-token")
			response := httptest.NewRecorder()
			invalidHandler.ServeHTTP(response, request)
			if response.Code != http.StatusServiceUnavailable || response.Header().Get("Cache-Control") != "no-store" || !containsJSONCode(response.Body.Bytes(), "BILLING_APPROVAL_ACTIVATION_UNAVAILABLE") {
				t.Fatalf("invalid repository projection escaped: %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestApprovalCommandActivationRequiresExactSignedWorkloadPermission(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var issuer *httptest.Server
	issuer = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/jwks" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "billing-activation", Algorithm: "RS256", Use: "sig"}}})
	}))
	t.Cleanup(issuer.Close)
	caFile := filepath.Join(t.TempDir(), "issuer-ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: issuer.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("APP_ENV", "production")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", caFile)
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", issuer.URL)
	t.Setenv("WORKLOAD_IDENTITY_JWKS_URL", issuer.URL+"/jwks")
	t.Setenv("WORKLOAD_IDENTITY_AUDIENCE", "billing-activation-test")
	token := signedBillingActivationToken(t, key, issuer.URL, "billing-activation-test", "system:serviceaccount:pinvou:account-service")
	snapshot := validBillingApprovalActivationSnapshot(time.Now().UTC())

	t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", `[{"subject":"system:serviceaccount:pinvou:account-service","service":"account-service","permissions":["internal.billing.approval-activation.read"]}]`)
	allowedRepository := &approvalActivationStub{snapshot: snapshot}
	allowedHandler := NewService(allowedRepository).Handler("")
	allowed := httptest.NewRequest(http.MethodGet, "/internal/v1/approval-command-activations", nil)
	allowed.Header.Set("Authorization", "Bearer "+token)
	allowedResponse := httptest.NewRecorder()
	allowedHandler.ServeHTTP(allowedResponse, allowed)
	if allowedResponse.Code != http.StatusOK || allowedRepository.calls.Load() != 1 {
		t.Fatalf("exact workload permission was not allowed: status=%d calls=%d body=%s", allowedResponse.Code, allowedRepository.calls.Load(), allowedResponse.Body.String())
	}
	otherWorkload := httptest.NewRequest(http.MethodGet, "/internal/v1/approval-command-activations", nil)
	otherWorkload.Header.Set("Authorization", "Bearer "+signedBillingActivationToken(t, key, issuer.URL, "billing-activation-test", "system:serviceaccount:pinvou:token-gateway"))
	otherWorkloadResponse := httptest.NewRecorder()
	allowedHandler.ServeHTTP(otherWorkloadResponse, otherWorkload)
	if otherWorkloadResponse.Code != http.StatusForbidden || allowedRepository.calls.Load() != 1 {
		t.Fatalf("unassigned workload identity reached projection: status=%d calls=%d body=%s", otherWorkloadResponse.Code, allowedRepository.calls.Load(), otherWorkloadResponse.Body.String())
	}

	t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", `[{"subject":"system:serviceaccount:pinvou:account-service","service":"account-service","permissions":["internal.billing.wallet.read"]}]`)
	deniedRepository := &approvalActivationStub{snapshot: snapshot}
	deniedHandler := NewService(deniedRepository).Handler("")
	denied := httptest.NewRequest(http.MethodGet, "/internal/v1/approval-command-activations", nil)
	denied.Header.Set("Authorization", "Bearer "+token)
	deniedResponse := httptest.NewRecorder()
	deniedHandler.ServeHTTP(deniedResponse, denied)
	if deniedResponse.Code != http.StatusForbidden || deniedRepository.calls.Load() != 0 {
		t.Fatalf("unassigned workload permission reached projection: status=%d calls=%d body=%s", deniedResponse.Code, deniedRepository.calls.Load(), deniedResponse.Body.String())
	}
}

func signedBillingActivationToken(t *testing.T, key *rsa.PrivateKey, issuer, audience, subject string) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "billing-activation"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	payload, err := json.Marshal(map[string]any{"iss": issuer, "sub": subject, "aud": audience, "iat": now.Unix(), "exp": now.Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	value, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := value.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func validBillingApprovalActivationSnapshot(now time.Time) ApprovalCommandActivationSnapshot {
	result := ApprovalCommandActivationSnapshot{SchemaVersion: 1, Activations: make([]ApprovalCommandActivation, 0, len(billingApprovalActivationOrder))}
	for _, commandType := range billingApprovalActivationOrder {
		spec := billingApprovalActivationSpecs[commandType]
		state := "INACTIVE"
		if spec.mayBeActive {
			state = "ACTIVE"
		}
		result.Activations = append(result.Activations, ApprovalCommandActivation{
			CommandType: commandType, PolicyVersion: spec.policyVersion, SnapshotSchemaVersion: spec.snapshotSchemaVersion,
			State: state, Version: 1, UpdatedAt: now,
		})
	}
	return result
}

func TestEntitlementCorrectionActivationV2MayBeActive(t *testing.T) {
	now := time.Now().UTC()
	snapshot := validBillingApprovalActivationSnapshot(now)
	item := &snapshot.Activations[1]
	if item.CommandType != entitlementComponentAdjustmentCommand {
		t.Fatal("unexpected activation order")
	}
	item.PolicyVersion, item.SnapshotSchemaVersion = 2, 2
	if err := validateApprovalCommandActivationSnapshot(snapshot, now); err != nil {
		t.Fatalf("inactive v2 should be readable: %v", err)
	}
	item.State = "ACTIVE"
	if err := validateApprovalCommandActivationSnapshot(snapshot, now); err != nil {
		t.Fatalf("activated v2 projection rejected: %v", err)
	}
	// Only the appended v2 projection may be activated: an ACTIVE v1 history
	// row and an unknown future version still fail the whole projection closed.
	item.PolicyVersion, item.SnapshotSchemaVersion, item.State = 1, 1, "ACTIVE"
	if err := validateApprovalCommandActivationSnapshot(snapshot, now); !errors.Is(err, ErrApprovalActivationUnavailable) {
		t.Fatalf("active v1 history accepted: %v", err)
	}
	item.State, item.PolicyVersion, item.SnapshotSchemaVersion = "INACTIVE", 3, 2
	if err := validateApprovalCommandActivationSnapshot(snapshot, now); !errors.Is(err, ErrApprovalActivationUnavailable) {
		t.Fatalf("unknown version accepted: %v", err)
	}
}

func containsJSONCode(raw []byte, expected string) bool {
	var body struct {
		Code string `json:"code"`
	}
	return json.Unmarshal(raw, &body) == nil && body.Code == expected
}
