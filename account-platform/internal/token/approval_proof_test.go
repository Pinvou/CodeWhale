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
	evidence.RouteSetFingerprint = strings.Repeat("e", 64)
	service.WithImpactPrechecks(true, stubImpactRepository{evidence: evidence}, nil)
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
