package billing

import (
	"bytes"
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
	"testing"
	"time"

	accountdomain "github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"
)

type billingApprovalProofStub struct {
	Repository
	*billingProofFactsStub
	activation ApprovalCommandActivationSnapshot
	calls      int
}

func TestBillingProofRequiresSignedAccountIdentity(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	issuer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/jwks" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "billing-activation", Algorithm: "RS256", Use: "sig"}}})
	}))
	defer issuer.Close()
	caFile := filepath.Join(t.TempDir(), "issuer-ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: issuer.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("APP_ENV", "production")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", caFile)
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", issuer.URL)
	t.Setenv("WORKLOAD_IDENTITY_JWKS_URL", issuer.URL+"/jwks")
	t.Setenv("WORKLOAD_IDENTITY_AUDIENCE", "billing-proof-test")
	t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", `[{"subject":"account-subject","service":"account-service","permissions":["internal.billing.approval-proof.read"]},{"subject":"portal-subject","service":"token-credential-portal","permissions":["internal.billing.approval-proof.read"]}]`)

	adjustment := ManualAdjustment{ID: uuid.New(), WalletID: uuid.New(), AmountMicrocredits: 370, RequestedBy: uuid.New(), Version: 1, Status: "PENDING_APPROVAL"}
	reader := &billingApprovalProofStub{billingProofFactsStub: &billingProofFactsStub{adjustment: adjustment, wallet: Wallet{ID: adjustment.WalletID, Status: "ACTIVE"}}, activation: validBillingApprovalActivationSnapshot(time.Now().UTC())}
	server := httptest.NewServer(NewService(reader).Handler("legacy-static"))
	defer server.Close()
	marshal := func(value any) json.RawMessage { encoded, _ := json.Marshal(value); return encoded }
	artifacts, err := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{
		CommandType: "WALLET_ADJUSTMENT", PolicyVersion: 1, SnapshotSchemaVersion: 1,
		TargetResourceType: "MANUAL_ADJUSTMENT", TargetResourceID: adjustment.ID.String(),
		NormalizedCommand:        marshal(map[string]any{"adjustment_id": adjustment.ID, "wallet_id": adjustment.WalletID, "amount_microcredits": adjustment.AmountMicrocredits, "requested_by": adjustment.RequestedBy}),
		CriticalResourceVersions: marshal(map[string]any{"adjustment_version": 1}),
		FinancialSnapshot:        marshal(map[string]any{"wallet_id": adjustment.WalletID, "delta_microcredits": adjustment.AmountMicrocredits}),
	})
	if err != nil {
		t.Fatal(err)
	}
	sign := func(subject string) string {
		return signedBillingActivationToken(t, key, issuer.URL, "billing-proof-test", subject)
	}
	source, err := accountdomain.NewHTTPApprovalProofSource(server.URL, server.URL, sign("account-subject"))
	if err != nil {
		t.Fatal(err)
	}
	proof, err := source.Proof(context.Background(), uuid.New(), artifacts)
	if err != nil || proof.Response.Verdict != "ALLOW" || reader.calls != 2 {
		t.Fatalf("signed Account to Billing proof=%+v reads=%d err=%v", proof, reader.calls, err)
	}
	reader.adjustment.Version++
	denied, err := source.Proof(context.Background(), uuid.New(), artifacts)
	if !errors.Is(err, accountdomain.ErrApprovalProofRejected) || denied.Response.ReasonCode != "SNAPSHOT_MISMATCH" || denied.Response.DomainRevision == proof.Response.DomainRevision {
		t.Fatalf("signed Account domain denial=%+v err=%v", denied, err)
	}
	reader.adjustment.Version--
	beforeUnauthorized := reader.calls
	body, err := approvalencoding.EncodeProofRequest(uuid.New(), artifacts)
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
			request, _ := http.NewRequest(http.MethodPost, server.URL+"/internal/v1/approval-command-proofs", bytes.NewReader(body))
			request.Header.Set("Authorization", "Bearer "+scenario.bearer)
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != scenario.status || reader.calls != beforeUnauthorized {
				t.Fatalf("wrong workload reached Billing proof: status=%d reads=%d", response.StatusCode, reader.calls)
			}
		})
	}
}

func (s *billingApprovalProofStub) GetWallet(ctx context.Context, id uuid.UUID) (Wallet, error) {
	return s.billingProofFactsStub.GetWallet(ctx, id)
}

func (s *billingApprovalProofStub) ApprovalCommandActivations(context.Context) (ApprovalCommandActivationSnapshot, error) {
	s.calls++
	return s.activation, nil
}

func TestBillingApprovalProofRechecksActivationAndOwnerFacts(t *testing.T) {
	now := time.Now().UTC()
	adjustment := ManualAdjustment{ID: uuid.New(), WalletID: uuid.New(), AmountMicrocredits: 370, RequestedBy: uuid.New(), Version: 1, Status: "PENDING_APPROVAL"}
	reader := &billingApprovalProofStub{billingProofFactsStub: &billingProofFactsStub{adjustment: adjustment, wallet: Wallet{ID: adjustment.WalletID, Status: "ACTIVE"}}, activation: validBillingApprovalActivationSnapshot(now)}
	service := NewService(reader)
	marshal := func(value any) json.RawMessage {
		t.Helper()
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	input := approvalencoding.LegacyProofInput{
		CommandType: "WALLET_ADJUSTMENT", PolicyVersion: 1, SnapshotSchemaVersion: 1,
		TargetResourceType: "MANUAL_ADJUSTMENT", TargetResourceID: adjustment.ID.String(),
		NormalizedCommand:        marshal(map[string]any{"adjustment_id": adjustment.ID, "wallet_id": adjustment.WalletID, "amount_microcredits": adjustment.AmountMicrocredits, "requested_by": adjustment.RequestedBy}),
		CriticalResourceVersions: marshal(map[string]any{"adjustment_version": 1}),
		FinancialSnapshot:        marshal(map[string]any{"wallet_id": adjustment.WalletID, "delta_microcredits": adjustment.AmountMicrocredits}),
	}
	artifacts, err := approvalencoding.ConvertLegacyProofInput(input)
	if err != nil {
		t.Fatal(err)
	}
	request := approvalencoding.ProofRequest{ApprovalRequestID: uuid.New(), Artifacts: artifacts}
	proof, err := service.billingApprovalProof(context.Background(), request)
	if err != nil || proof.Verdict != "ALLOW" || proof.ReasonCode != "PROOF_VALID" || reader.calls != 2 {
		t.Fatalf("authoritative wallet proof=%+v calls=%d err=%v", proof, reader.calls, err)
	}
	if _, err := approvalencoding.DeriveProofDigest(proof); err != nil {
		t.Fatalf("response must meet the closed transport: %v", err)
	}
	staleInput := input
	staleInput.CriticalResourceVersions = marshal(map[string]any{"adjustment_version": 2})
	staleArtifacts, err := approvalencoding.ConvertLegacyProofInput(staleInput)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := service.billingApprovalProof(context.Background(), approvalencoding.ProofRequest{ApprovalRequestID: uuid.New(), Artifacts: staleArtifacts})
	if err != nil || stale.ReasonCode != "SNAPSHOT_MISMATCH" || stale.DomainRevision != proof.DomainRevision {
		t.Fatalf("same owner facts must retain domain revision across requests: %+v err=%v", stale, err)
	}
	reader.adjustment.Version++
	changed, err := service.billingApprovalProof(context.Background(), request)
	if err != nil || changed.Verdict != "DENY" || changed.ReasonCode != "SNAPSHOT_MISMATCH" || changed.DomainRevision == proof.DomainRevision {
		t.Fatalf("owner version drift=%+v err=%v", changed, err)
	}
	reader.adjustment.Version--
	reader.adjustment.Status = "POSTED"
	posted, err := service.billingApprovalProof(context.Background(), request)
	if err != nil || posted.Verdict != "DENY" || posted.ReasonCode != "RESOURCE_STATE_CHANGED" || posted.DomainRevision == proof.DomainRevision {
		t.Fatalf("owner status drift=%+v err=%v", posted, err)
	}
	for i := range reader.activation.Activations {
		if reader.activation.Activations[i].CommandType == "WALLET_ADJUSTMENT" {
			reader.activation.Activations[i].State = "INACTIVE"
			reader.activation.Activations[i].Version++
		}
	}
	if _, err := service.billingApprovalProof(context.Background(), request); err == nil {
		t.Fatal("inactive Billing activation issued a proof")
	}
}
