package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	billingdomain "github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/google/uuid"
)

// TestEntitlementCorrectionCrossDomainGatePostgresIntegration runs the real
// Account and Billing databases with the real Billing HTTP proof authority and
// proves the cross-domain gate stays closed until the command family is
// separately activated: Account has no executable implementation, the Billing
// owner activation row is INACTIVE, the real proof authority refuses to issue
// proof, a premature schema-owner activation fails the whole Billing activation
// projection closed, and neither domain records any effect. The positive
// Account-to-Billing approval/execution E2E requires the activation capability
// and is therefore not claimed here.
func TestEntitlementCorrectionCrossDomainGatePostgresIntegration(t *testing.T) {
	accountURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	billingURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if accountURL == "" || billingURL == "" {
		t.Skip("real Account and Billing PostgreSQL URLs are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)

	repository, _, _ := newTokenOwnerCandidateIntegrationRepository(t, ctx, accountURL, 0)
	billingRepository, billingPool, billingConnection := newEntitlementCrossDomainBillingFixture(t, ctx, billingURL)
	billingServer := httptest.NewServer(billingdomain.NewService(billingRepository).Handler("account-proof-test"))
	t.Cleanup(billingServer.Close)
	proofSource, err := NewHTTPApprovalProofSource("http://127.0.0.1", billingServer.URL, "account-proof-test")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := approvalpolicy.Lookup("ENTITLEMENT_COMPONENT_ADJUSTMENT")
	if err != nil {
		t.Fatal(err)
	}
	if policy.PolicyVersion != 2 || policy.SnapshotSchemaVersion != 2 {
		t.Fatalf("unexpected entitlement correction policy metadata: %#v", policy)
	}

	// Billing keeps the appended v2 row INACTIVE next to the immutable v1 row.
	activations, err := billingRepository.ApprovalCommandActivations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var activation billingdomain.ApprovalCommandActivation
	for _, item := range activations.Activations {
		if item.CommandType == policy.CommandType {
			activation = item
		}
	}
	if activation.State != "INACTIVE" || activation.PolicyVersion != 2 || activation.SnapshotSchemaVersion != 2 {
		t.Fatalf("entitlement correction activation is not the deployed INACTIVE v2 row: %#v", activation)
	}
	var retainedV1 int64
	if err := billingPool.QueryRow(ctx, `SELECT count(*) FROM billing_approval_command_activations
WHERE command_type='ENTITLEMENT_COMPONENT_ADJUSTMENT' AND policy_version=1 AND snapshot_schema_version=1 AND state='INACTIVE'`).Scan(&retainedV1); err != nil || retainedV1 != 1 {
		t.Fatalf("Billing retained v1 activation history=%d err=%v", retainedV1, err)
	}

	// The real Billing proof authority refuses to issue proof for the closed
	// owner projection while the command is not activated.
	frozen, err := billingRepository.EntitlementCorrectionProofInput(ctx, entitlementCrossDomainPackageID, entitlementCrossDomainOrderID, "ORIGINAL_SPLIT_MISMATCH")
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := approvalencoding.ConvertLegacyProofInput(frozen)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := proofSource.Proof(ctx, uuid.New(), artifacts); !errors.Is(err, ErrApprovalProofUnavailable) {
		t.Fatalf("inactive cross-domain proof did not fail closed: %v", err)
	}

	// Account refuses every mutating entry point before it reads Billing
	// activation, because the command has no executable implementation yet.
	activationReader := &billingAdvancementReader{snapshot: billingAdvancementSnapshot("ACTIVE")}
	repository.WithApprovalProofSource(proofSource).WithBillingGovernanceActivation(activationReader)
	requester := seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PLATFORM_SUPER_ADMIN", "entitlement-gate-requester")
	approver := seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PLATFORM_SUPER_ADMIN", "entitlement-gate-approver")
	requesterPrincipal, approverPrincipal := approvalIntegrationPrincipal(requester), approvalIntegrationPrincipal(approver)

	// The advancement guard is the precise gate; the create entry point maps the
	// same closed state to its public conflict result.
	if err := repository.requireApprovalAdvancement(ctx, policy); !errors.Is(err, ErrApprovalCommandNotExecutable) {
		t.Fatalf("closed command reported executable advancement: %v", err)
	}
	createDraft := ApprovalDraft{
		CommandType: policy.CommandType, IdempotencyKey: "entitlement-gate-create",
		NormalizedCommand: frozen.NormalizedCommand, TargetResourceType: frozen.TargetResourceType,
		TargetResourceID: frozen.TargetResourceID, Reason: "closed cross-domain gate create",
		RedactedEvidence: json.RawMessage(`[]`), ExpectedImpact: json.RawMessage(`{}`),
		CriticalResourceVersions: frozen.CriticalResourceVersions, FinancialSnapshot: frozen.FinancialSnapshot,
	}
	if _, err := repository.CreateApprovalDraft(ctx, requesterPrincipal, createDraft); !errors.Is(err, ErrConflict) {
		t.Fatalf("inactive cross-domain create: %v", err)
	}
	assertApprovalRequestAbsent(t, ctx, repository, requester.ID, createDraft.IdempotencyKey)

	submitID := seedAdvancementRequest(t, ctx, repository, requester.ID, policy, "DRAFT")
	if _, err := repository.SubmitApprovalRequest(ctx, requesterPrincipal, submitID, 1); !errors.Is(err, ErrApprovalCommandNotExecutable) {
		t.Fatalf("inactive cross-domain submit: %v", err)
	}
	assertApprovalRequestState(t, ctx, repository, submitID, "DRAFT", 1)

	updateID := seedAdvancementRequest(t, ctx, repository, requester.ID, policy, "DRAFT")
	updateDraft := createDraft
	updateDraft.IdempotencyKey = "entitlement-gate-update"
	if _, err := repository.UpdateApprovalDraft(ctx, requesterPrincipal, updateID, 1, updateDraft); !errors.Is(err, ErrConflict) {
		t.Fatalf("inactive cross-domain update: %v", err)
	}
	assertApprovalRequestState(t, ctx, repository, updateID, "DRAFT", 1)

	approveID := seedAdvancementRequest(t, ctx, repository, requester.ID, policy, "SUBMITTED")
	if _, err := repository.DecideApprovalRequest(ctx, approverPrincipal, approveID, 1, "APPROVED", "closed cross-domain gate", frozen.CriticalResourceVersions); !errors.Is(err, ErrApprovalCommandNotExecutable) {
		t.Fatalf("inactive cross-domain approve: %v", err)
	}
	assertApprovalRequestState(t, ctx, repository, approveID, "SUBMITTED", 1)
	if activationReader.calls != 0 {
		t.Fatalf("closed command reached Billing activation %d times", activationReader.calls)
	}

	// The executor entry point fails closed as well; the seeded approval has no
	// frozen consumer binding because the command cannot be approved at all, so
	// any begin attempt must leave the request and the execution table untouched.
	beginID := seedAdvancementRequest(t, ctx, repository, requester.ID, policy, "APPROVED")
	if _, err := repository.BeginApprovalExecution(ctx, beginID, policy.ExecutorIdentity, "entitlement-gate-job", json.RawMessage(`{}`), json.RawMessage(`{}`)); err == nil {
		t.Fatal("inactive cross-domain begin unexpectedly succeeded")
	}
	assertApprovalRequestState(t, ctx, repository, beginID, "APPROVED", 1)
	var beginExecutions int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM approval_executions WHERE approval_request_id=$1`, beginID).Scan(&beginExecutions); err != nil || beginExecutions != 0 {
		t.Fatalf("closed cross-domain begin recorded %d executions: %v", beginExecutions, err)
	}

	var decisions, outbox, executions int
	if err := repository.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM approval_decisions WHERE approval_request_id=$1),
		(SELECT count(*) FROM outbox_events WHERE aggregate_id=$1),
		(SELECT count(*) FROM approval_executions WHERE approval_request_id=$1)`, approveID).Scan(&decisions, &outbox, &executions); err != nil || decisions != 0 || outbox != 0 || executions != 0 {
		t.Fatalf("closed cross-domain approval created effects: %d/%d/%d err=%v", decisions, outbox, executions, err)
	}
	var corrections, claims, bonusComponents int64
	var sourceAvailable, sourceReclassified, sourceOriginal int64
	if err := billingPool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM entitlement_component_corrections),
		(SELECT count(*) FROM billing_approval_proof_claims),
		(SELECT count(*) FROM entitlement_components WHERE package_id=$1 AND component_type='BONUS'),
		COALESCE((SELECT available_microcredits FROM entitlement_components WHERE id=$2),0),
		COALESCE((SELECT reclassified_out_microcredits FROM entitlement_components WHERE id=$2),0),
		COALESCE((SELECT original_microcredits FROM entitlement_components WHERE id=$2),0)`, entitlementCrossDomainPackageID, entitlementCrossDomainComponentID).
		Scan(&corrections, &claims, &bonusComponents, &sourceAvailable, &sourceReclassified, &sourceOriginal); err != nil ||
		corrections != 0 || claims != 0 || bonusComponents != 0 || sourceAvailable != sourceOriginal || sourceReclassified != 0 || sourceOriginal != 100 {
		t.Fatalf("closed cross-domain gate wrote Billing effects: corrections=%d claims=%d bonus=%d source=%d/%d/%d err=%v",
			corrections, claims, bonusComponents, sourceAvailable, sourceReclassified, sourceOriginal, err)
	}

	// A premature schema-owner activation must fail the whole projection closed
	// instead of activating a single command, so the reader cannot be talked
	// into partial activation before the governed release step.
	if _, err := billingConnection.Exec(ctx, `SET ROLE billing_schema_owner;
UPDATE billing_approval_command_activations SET state='ACTIVE',version=version+1
WHERE command_type='ENTITLEMENT_COMPONENT_ADJUSTMENT' AND policy_version=2 AND snapshot_schema_version=2 AND state='INACTIVE'`); err != nil {
		t.Fatal(err)
	}
	if _, err := billingRepository.ApprovalCommandActivations(ctx); !errors.Is(err, billingdomain.ErrApprovalActivationUnavailable) {
		t.Fatalf("premature cross-domain activation did not fail the projection closed: %v", err)
	}
	if _, err := billingRepository.EntitlementCorrectionProofInput(ctx, entitlementCrossDomainPackageID, entitlementCrossDomainOrderID, "ORIGINAL_SPLIT_MISMATCH"); err != nil {
		t.Fatalf("owner read path regressed after premature activation: %v", err)
	}
}
