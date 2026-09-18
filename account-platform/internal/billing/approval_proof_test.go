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
	"strings"
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

// The correction command is recognized by the proof path but must stay
// unavailable for as long as its owner activation is still INACTIVE: the
// activation validator only accepts the v2 pair while it remains deactivated.
func TestEntitlementCorrectionProofStaysUnavailableWhileInactive(t *testing.T) {
	now := time.Now().UTC()
	snapshot := validBillingApprovalActivationSnapshot(now)
	snapshot.Activations[1].PolicyVersion, snapshot.Activations[1].SnapshotSchemaVersion = 2, 2
	if err := validateApprovalCommandActivationSnapshot(snapshot, now); err != nil {
		t.Fatalf("inactive v2 correction projection must stay readable: %v", err)
	}
	packageID, orderID := uuid.New(), uuid.New()
	marshal := func(value any) json.RawMessage {
		t.Helper()
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	artifacts, err := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{
		CommandType: entitlementComponentAdjustmentCommand, PolicyVersion: 2, SnapshotSchemaVersion: 2,
		TargetResourceType: "ENTITLEMENT_PACKAGE", TargetResourceID: packageID.String(),
		NormalizedCommand: marshal(map[string]any{"package_id": packageID, "payment_order_id": orderID, "reason_code": "SNAPSHOT_SPLIT_MISMATCH"}),
		CriticalResourceVersions: marshal(map[string]any{
			"package_id": packageID, "wallet_id": uuid.New(), "wallet_version": 3, "payment_order_id": orderID,
			"payment_transaction_id": uuid.New(), "source_component_id": uuid.New(), "source_lot_id": uuid.New(),
			"target_component_id": nil, "target_lot_id": nil, "source_type": "BASE", "target_type": "BONUS",
			"amount_microcredits": 20, "source_evidence_digest": strings.Repeat("ab", 32),
			"facts_digest": strings.Repeat("cd", 32), "policy_version": 2,
		}),
		FinancialSnapshot: marshal(map[string]any{
			"package_total_microcredits": 100, "source_original_microcredits": 70,
			"target_original_microcredits": 30, "effective_total_microcredits": 100,
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	reader := &billingApprovalProofStub{billingProofFactsStub: &billingProofFactsStub{}, activation: snapshot}
	service := NewService(reader)
	request := approvalencoding.ProofRequest{ApprovalRequestID: uuid.New(), Artifacts: artifacts}
	if _, err := service.billingApprovalProof(context.Background(), request); !errors.Is(err, errBillingApprovalProofUnavailable) {
		t.Fatalf("inactive correction proof must stay unavailable: %v", err)
	}
	// The capability itself only clears the activation gate: an activated v2
	// projection is selectable, while proof still requires real owner facts.
	if _, err := activeBillingProofCommand(snapshot, entitlementComponentAdjustmentCommand, time.Now().UTC()); !errors.Is(err, errBillingApprovalProofUnavailable) {
		t.Fatalf("inactive activation stayed selectable: %v", err)
	}
	activated := snapshot
	activated.Activations[1].State = "ACTIVE"
	selected, err := activeBillingProofCommand(activated, entitlementComponentAdjustmentCommand, time.Now().UTC())
	if err != nil || selected.State != "ACTIVE" || selected.PolicyVersion != 2 || selected.SnapshotSchemaVersion != 2 {
		t.Fatalf("activated v2 projection is not selectable: %+v err=%v", selected, err)
	}
	reader.activation = activated
	response, err := service.billingApprovalProof(context.Background(), request)
	if err != nil && !errors.Is(err, errBillingApprovalProofUnavailable) {
		t.Fatalf("activation without owner facts returned an unexpected error: %v", err)
	}
	if err == nil && response.Verdict != "DENY" {
		t.Fatalf("activation without matching owner facts issued proof: %+v", response)
	}
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
