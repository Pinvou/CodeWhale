package token

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
)

func TestTokenApprovalProofRecomputesOwnerFactsAndEpoch(t *testing.T) {
	accountID, credentialID := uuid.New(), uuid.New()
	accountVersion, credentialVersion := int64(7), 3
	evidence := ImpactLocalEvidence{
		ResourceType: "PROVIDER_CREDENTIAL", ResourceID: credentialID.String(), CurrentStatus: "ACTIVE",
		CurrentVersion: ImpactResourceVersion{AccountVersion: &accountVersion, CredentialVersion: &credentialVersion},
		ObservedAt:     time.Now().UTC(), CredentialFingerprint: strings.Repeat("a", 64),
		RouteSetFingerprint: strings.Repeat("b", 64), ProvisioningSetFingerprint: strings.Repeat("c", 64),
		InflightSetFingerprint: strings.Repeat("d", 64),
	}
	service := NewService(nil, nil, nil, nil, ProviderURLPolicy{}, nil, nil, 0).
		WithImpactPrechecks(true, stubImpactRepository{evidence: evidence}, nil).
		WithTokenCommandGovernanceActivation(testTokenGovernanceActivationReader{activation: TokenCommandGovernanceActivation{State: "ACTIVE", Epoch: 7}})
	command := json.RawMessage(`{"token_account_id":"` + accountID.String() + `","expected_account_version":7,"credential_id":"` + credentialID.String() + `","credential_version":3}`)
	projection, err := service.approvalCommandPrecheck(context.Background(), ApprovalSnapshotRequest{CommandType: "TOKEN_CREDENTIAL_REVOKE", TargetResourceType: "PROVIDER_CREDENTIAL", TargetResourceID: credentialID.String(), NormalizedCommand: command})
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{
		CommandType: "TOKEN_CREDENTIAL_REVOKE", PolicyVersion: 1, SnapshotSchemaVersion: 1,
		TargetResourceType: "PROVIDER_CREDENTIAL", TargetResourceID: credentialID.String(), NormalizedCommand: command,
		CriticalResourceVersions: projection.CriticalResourceVersions, FinancialSnapshot: projection.FinancialSnapshot,
	})
	if err != nil {
		t.Fatal(err)
	}
	requestID := uuid.New()
	request := approvalencoding.ProofRequest{ApprovalRequestID: requestID, Artifacts: artifacts}
	proof, err := service.tokenApprovalProof(context.Background(), request)
	if err != nil || proof.Verdict != "ALLOW" || proof.DomainRevision != "7:"+projection.PrecheckFingerprint || !approvalencoding.ProofResponseBound(proof, requestID, artifacts) {
		t.Fatalf("authoritative proof=%+v err=%v", proof, err)
	}
	proofBody := approvedTokenCommandBody{
		ActivationEpoch: 7, ProofSchemaVersion: proof.ProofSchemaVersion,
		CommandDigest: proof.CommandDigest, SnapshotDigest: proof.SnapshotDigest,
		DomainRevision: proof.DomainRevision, ProofDigest: strings.Repeat("f", 64), ApprovalControlEpoch: 3,
		CriticalResourceVersions: projection.CriticalResourceVersions, FinancialSnapshot: projection.FinancialSnapshot,
		Envelope: TokenCommandEnvelope{CommandType: "TOKEN_CREDENTIAL_REVOKE", TargetResourceType: "PROVIDER_CREDENTIAL", TargetResourceID: credentialID.String(), Command: command},
	}
	selection := tokenRouteActivationSelection{TokenCommandGovernanceActivation: TokenCommandGovernanceActivation{State: "ACTIVE", Epoch: 7}, transactional: true}
	if err := service.tokenExecutionProof(context.Background(), "TOKEN_CREDENTIAL_REVOKE", proofBody, selection); err != nil {
		t.Fatalf("unchanged owner revision rejected: %v", err)
	}
	malformed := proofBody
	malformed.SnapshotDigest = strings.Repeat("0", 64)
	if service.tokenExecutionProof(context.Background(), "TOKEN_CREDENTIAL_REVOKE", malformed, selection) == nil {
		t.Fatal("altered frozen snapshot digest accepted")
	}
	malformed = proofBody
	malformed.DomainRevision = "8:" + projection.PrecheckFingerprint
	if service.tokenExecutionProof(context.Background(), "TOKEN_CREDENTIAL_REVOKE", malformed, selection) == nil {
		t.Fatal("different Token epoch accepted")
	}
	evidence.RouteSetFingerprint = strings.Repeat("e", 64)
	service.WithImpactPrechecks(true, stubImpactRepository{evidence: evidence}, nil)
	if service.tokenExecutionProof(context.Background(), "TOKEN_CREDENTIAL_REVOKE", proofBody, selection) == nil {
		t.Fatal("changed owner facts were executed")
	}
	proof, err = service.tokenApprovalProof(context.Background(), request)
	if err != nil || proof.Verdict != "DENY" || proof.ReasonCode != "SNAPSHOT_MISMATCH" {
		t.Fatalf("drift proof=%+v err=%v", proof, err)
	}
	service.WithTokenCommandGovernanceActivation(testTokenGovernanceActivationReader{activation: TokenCommandGovernanceActivation{State: "SAFE_ROLLBACK", Epoch: 8}})
	if _, err := service.tokenApprovalProof(context.Background(), request); err == nil {
		t.Fatal("SAFE_ROLLBACK issued a proof")
	}
}

func TestTokenApprovalProofRouteDoesNotTrustLegacyTokenOrBrowser(t *testing.T) {
	service := NewService(nil, nil, nil, nil, ProviderURLPolicy{}, nil, nil, 0)
	for _, item := range []struct {
		name, bearer string
		status       int
	}{
		{"browser", "", http.StatusUnauthorized},
		{"legacy shared token", "Bearer legacy", http.StatusForbidden},
	} {
		t.Run(item.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/internal/v1/approval-command-proofs", strings.NewReader(`{}`))
			if item.bearer != "" {
				request.Header.Set("Authorization", item.bearer)
			}
			response := httptest.NewRecorder()
			service.Handler("legacy", nil).ServeHTTP(response, request)
			if response.Code != item.status || !strings.Contains(response.Body.String(), "SERVICE_AUTH_") {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestTokenProofSnapshotIgnoresOnlyPriceObservationClock(t *testing.T) {
	critical := json.RawMessage(`{"catalog_version":7}`)
	financial := json.RawMessage(`{"billing_price_revision":11,"price_set_fingerprint":"` + strings.Repeat("a", 64) + `","models_checked":2,"models_covered":2,"fully_covered":true,"billing_as_of":"2026-09-16T01:00:00Z"}`)
	frozen := approvalencoding.LegacyProofInput{CriticalResourceVersions: critical, FinancialSnapshot: financial}
	current := ApprovalSnapshotProjection{CriticalResourceVersions: critical,
		FinancialSnapshot: json.RawMessage(strings.Replace(string(financial), "01:00:00Z", "01:01:00Z", 1))}
	if !tokenProofSnapshotMatches("TOKEN_MODEL_CATALOG_ROLLBACK", frozen, current) {
		t.Fatal("observation clock was mistaken for domain drift")
	}
	current.FinancialSnapshot = json.RawMessage(strings.Replace(string(current.FinancialSnapshot), `"billing_price_revision":11`, `"billing_price_revision":12`, 1))
	if tokenProofSnapshotMatches("TOKEN_MODEL_CATALOG_ROLLBACK", frozen, current) {
		t.Fatal("price revision drift was ignored")
	}
	current.FinancialSnapshot = financial
	current.CriticalResourceVersions = json.RawMessage(`{"catalog_version":8}`)
	if tokenProofSnapshotMatches("TOKEN_MODEL_CATALOG_ROLLBACK", frozen, current) {
		t.Fatal("Token catalog drift was ignored")
	}
	frozen.CriticalResourceVersions = json.RawMessage(`{"catalog_version":9007199254740992}`)
	current.CriticalResourceVersions = json.RawMessage(`{"catalog_version":9007199254740993}`)
	if tokenProofSnapshotMatches("TOKEN_MODEL_CATALOG_ROLLBACK", frozen, current) {
		t.Fatal("adjacent int64 versions collapsed to the same float64")
	}
}
