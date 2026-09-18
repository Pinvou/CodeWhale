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
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/billinggovernance"
)

// TestEntitlementCorrectionCrossDomainActivationPostgresIntegration runs the
// real Account and Billing databases with the real Billing HTTP activation and
// proof authority. It proves the capability is wired without opening anything by
// default: while the deployed activation row is INACTIVE the Account entry
// points refuse with the Billing governance error, the proof authority refuses
// to issue proof and neither domain records an effect; once a schema-owner
// activation flips the appended v2 row, the Account reader and the real proof
// authority both honor it, and flipping the row back closes the command again.
func TestEntitlementCorrectionCrossDomainActivationPostgresIntegration(t *testing.T) {
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
	activationReader, err := billinggovernance.NewClient(billingServer.URL, "account-proof-test")
	if err != nil {
		t.Fatal(err)
	}
	repository.WithBillingGovernanceActivation(activationReader)
	policy, err := approvalpolicy.Lookup("ENTITLEMENT_COMPONENT_ADJUSTMENT")
	if err != nil {
		t.Fatal(err)
	}
	if policy.PolicyVersion != 2 || policy.SnapshotSchemaVersion != 2 || !approvalpolicy.Executable(policy.CommandType) {
		t.Fatalf("entitlement correction must be registered against the live Billing activation: %#v", policy)
	}

	// The deployed projection stays INACTIVE and keeps the immutable v1 history.
	activations, err := billingRepository.ApprovalCommandActivations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	activation := activations.Activations[1]
	if activation.CommandType != policy.CommandType || activation.State != "INACTIVE" || activation.PolicyVersion != 2 || activation.SnapshotSchemaVersion != 2 {
		t.Fatalf("entitlement correction activation is not the deployed INACTIVE v2 row: %#v", activation)
	}
	var retainedV1 int64
	if err := billingPool.QueryRow(ctx, `SELECT count(*) FROM billing_approval_command_activations
WHERE command_type='ENTITLEMENT_COMPONENT_ADJUSTMENT' AND policy_version=1 AND snapshot_schema_version=1 AND state='INACTIVE'`).Scan(&retainedV1); err != nil || retainedV1 != 1 {
		t.Fatalf("Billing retained v1 activation history=%d err=%v", retainedV1, err)
	}

	frozen, err := billingRepository.EntitlementCorrectionProofInput(ctx, entitlementCrossDomainPackageID, entitlementCrossDomainOrderID, "ORIGINAL_SPLIT_MISMATCH")
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.requireApprovalAdvancement(ctx, policy); !errors.Is(err, ErrBillingGovernanceInactive) {
		t.Fatalf("inactive cross-domain advancement: %v", err)
	}

	requester := seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PLATFORM_SUPER_ADMIN", "entitlement-activation-requester")
	approver := seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PLATFORM_SUPER_ADMIN", "entitlement-activation-approver")
	requesterPrincipal, approverPrincipal := approvalIntegrationPrincipal(requester), approvalIntegrationPrincipal(approver)
	createDraft := ApprovalDraft{
		CommandType: policy.CommandType, IdempotencyKey: "entitlement-activation-create",
		NormalizedCommand: frozen.NormalizedCommand, TargetResourceType: frozen.TargetResourceType,
		TargetResourceID: frozen.TargetResourceID, Reason: "closed cross-domain activation gate",
		RedactedEvidence: json.RawMessage(`[]`), ExpectedImpact: json.RawMessage(`{}`),
		CriticalResourceVersions: frozen.CriticalResourceVersions, FinancialSnapshot: frozen.FinancialSnapshot,
	}
	if _, err := repository.CreateApprovalDraft(ctx, requesterPrincipal, createDraft); err == nil {
		t.Fatal("inactive cross-domain create unexpectedly succeeded")
	}
	assertApprovalRequestAbsent(t, ctx, repository, requester.ID, createDraft.IdempotencyKey)

	submitID := seedAdvancementRequest(t, ctx, repository, requester.ID, policy, "DRAFT")
	if _, err := repository.SubmitApprovalRequest(ctx, requesterPrincipal, submitID, 1); !errors.Is(err, ErrBillingGovernanceInactive) {
		t.Fatalf("inactive cross-domain submit: %v", err)
	}
	assertApprovalRequestState(t, ctx, repository, submitID, "DRAFT", 1)

	approveID := seedAdvancementRequest(t, ctx, repository, requester.ID, policy, "SUBMITTED")
	if _, err := repository.DecideApprovalRequest(ctx, approverPrincipal, approveID, 1, "APPROVED", "closed cross-domain gate", frozen.CriticalResourceVersions); !errors.Is(err, ErrBillingGovernanceInactive) {
		t.Fatalf("inactive cross-domain approve: %v", err)
	}
	assertApprovalRequestState(t, ctx, repository, approveID, "SUBMITTED", 1)

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

	// Activation capability: a governed schema-owner flip of the appended v2 row
	// is honored by the Account reader and the real proof authority, without
	// writing any domain effect by itself, and the v1 history stays untouched.
	if _, err := billingConnection.Exec(ctx, `SET ROLE billing_schema_owner;
UPDATE billing_approval_command_activations SET state='ACTIVE',version=version+1
WHERE command_type='ENTITLEMENT_COMPONENT_ADJUSTMENT' AND policy_version=2 AND snapshot_schema_version=2 AND state='INACTIVE'`); err != nil {
		t.Fatal(err)
	}
	if err := repository.requireApprovalAdvancement(ctx, policy); err != nil {
		t.Fatalf("activated cross-domain advancement: %v", err)
	}
	snapshot, err := activationReader.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	projected, ok := snapshot.Find(policy.CommandType, 2, 2)
	if !ok || projected.State != "ACTIVE" {
		t.Fatalf("Account reader did not observe the activated projection: %#v", projected)
	}

	// Rolling the activation row back closes the command again with no history
	// rewrite: the same v1 row and the same v2 identity stay in place.
	if _, err := billingConnection.Exec(ctx, `SET ROLE billing_schema_owner;
UPDATE billing_approval_command_activations SET state='INACTIVE',version=version+1
WHERE command_type='ENTITLEMENT_COMPONENT_ADJUSTMENT' AND policy_version=2 AND snapshot_schema_version=2 AND state='ACTIVE'`); err != nil {
		t.Fatal(err)
	}
	if err := repository.requireApprovalAdvancement(ctx, policy); !errors.Is(err, ErrBillingGovernanceInactive) {
		t.Fatalf("rolled-back cross-domain advancement: %v", err)
	}
	rolledBack, err := activationReader.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if projected, ok := rolledBack.Find(policy.CommandType, 2, 2); !ok || projected.State != "INACTIVE" {
		t.Fatalf("Account reader did not observe the rolled-back projection: %#v", projected)
	}
}
