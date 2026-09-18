package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/billinggovernance"
	"github.com/google/uuid"
)

type billingActivationOwnerStub struct {
	billing.Repository
	mu       sync.RWMutex
	snapshot billing.ApprovalCommandActivationSnapshot
	calls    atomic.Int64
}

func (s *billingActivationOwnerStub) ApprovalCommandActivations(context.Context) (billing.ApprovalCommandActivationSnapshot, error) {
	s.calls.Add(1)
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshot, nil
}

func (s *billingActivationOwnerStub) setSnapshot(snapshot billing.ApprovalCommandActivationSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot = snapshot
}

func billingOwnerSnapshot(state string) billing.ApprovalCommandActivationSnapshot {
	shared := billingAdvancementSnapshot(state)
	result := billing.ApprovalCommandActivationSnapshot{SchemaVersion: shared.SchemaVersion, Activations: make([]billing.ApprovalCommandActivation, 0, len(shared.Activations))}
	for _, item := range shared.Activations {
		result.Activations = append(result.Activations, billing.ApprovalCommandActivation{
			CommandType: item.CommandType, PolicyVersion: item.PolicyVersion, SnapshotSchemaVersion: item.SnapshotSchemaVersion,
			State: item.State, Version: item.Version, UpdatedAt: item.UpdatedAt,
		})
	}
	return result
}

func TestBillingApprovalActivationAccountPostgresIntegration(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	if dsn == "" {
		t.Skip("PINVOU_TEST_ACCOUNT_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	repository, _, _, _ := newTokenOwnerCandidateIntegrationRepository(t, ctx, dsn, 0)
	requester := seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PLATFORM_SUPER_ADMIN", "billing-activation-requester")
	approver := seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PLATFORM_SUPER_ADMIN", "billing-activation-approver")
	requesterPrincipal, approverPrincipal := approvalIntegrationPrincipal(requester), approvalIntegrationPrincipal(approver)
	policy, err := approvalpolicy.Lookup("WALLET_ADJUSTMENT")
	if err != nil {
		t.Fatal(err)
	}

	owner := &billingActivationOwnerStub{snapshot: billingOwnerSnapshot("INACTIVE")}
	server := httptest.NewServer(billing.NewService(owner).Handler("billing-activation-account-test"))
	t.Cleanup(server.Close)
	reader, err := billinggovernance.NewClient(server.URL, "billing-activation-account-test")
	if err != nil {
		t.Fatal(err)
	}
	repository.WithBillingGovernanceActivation(reader)

	draft := approvalTestDraft()
	draft.IdempotencyKey = "billing-activation-create-inactive"
	if _, err := repository.CreateApprovalDraft(ctx, requesterPrincipal, draft); !errors.Is(err, ErrBillingGovernanceInactive) {
		t.Fatalf("inactive create: %v", err)
	}
	assertApprovalRequestAbsent(t, ctx, repository, requester.ID, draft.IdempotencyKey)

	updateID := seedAdvancementRequest(t, ctx, repository, requester.ID, policy, "DRAFT")
	update := approvalTestDraft()
	update.IdempotencyKey = "billing-activation-update-inactive"
	if _, err := repository.UpdateApprovalDraft(ctx, requesterPrincipal, updateID, 1, update); !errors.Is(err, ErrBillingGovernanceInactive) {
		t.Fatalf("inactive update: %v", err)
	}
	assertApprovalRequestState(t, ctx, repository, updateID, "DRAFT", 1)

	submitID := seedAdvancementRequest(t, ctx, repository, requester.ID, policy, "DRAFT")
	if _, err := repository.SubmitApprovalRequest(ctx, requesterPrincipal, submitID, 1); !errors.Is(err, ErrBillingGovernanceInactive) {
		t.Fatalf("inactive submit: %v", err)
	}
	assertApprovalRequestState(t, ctx, repository, submitID, "DRAFT", 1)

	approveID := seedAdvancementRequest(t, ctx, repository, requester.ID, policy, "SUBMITTED")
	if _, err := repository.DecideApprovalRequest(ctx, approverPrincipal, approveID, 1, "APPROVED", "inactive Billing activation", json.RawMessage(`{}`)); !errors.Is(err, ErrBillingGovernanceInactive) {
		t.Fatalf("inactive approve: %v", err)
	}
	assertApprovalRequestState(t, ctx, repository, approveID, "SUBMITTED", 1)

	beginID := seedAdvancementRequest(t, ctx, repository, requester.ID, policy, "APPROVED")
	if _, err := repository.BeginApprovalExecution(ctx, beginID, policy.ExecutorIdentity, "billing-inactive-job", json.RawMessage(`{}`), json.RawMessage(`{}`)); !errors.Is(err, ErrBillingGovernanceInactive) {
		t.Fatalf("inactive begin: %v", err)
	}
	assertApprovalRequestState(t, ctx, repository, beginID, "APPROVED", 1)

	beforeSafeClosure := owner.calls.Load()
	withdrawID := seedAdvancementRequest(t, ctx, repository, requester.ID, policy, "SUBMITTED")
	if item, err := repository.WithdrawApprovalRequest(ctx, requesterPrincipal, withdrawID, 1); err != nil || item.Status != "WITHDRAWN" {
		t.Fatalf("inactive withdraw: %s %v", item.Status, err)
	}
	rejectID := seedAdvancementRequest(t, ctx, repository, requester.ID, policy, "SUBMITTED")
	if item, err := repository.DecideApprovalRequest(ctx, approverPrincipal, rejectID, 1, "REJECTED", "safe close inactive Billing command", nil); err != nil || item.Status != "REJECTED" {
		t.Fatalf("inactive reject: %s %v", item.Status, err)
	}
	if owner.calls.Load() != beforeSafeClosure {
		t.Fatal("safe closure unexpectedly read or depended on Billing activation")
	}

	historicalID := seedExecutingBillingApproval(t, ctx, repository, requester.ID, approver.ID, policy)
	beforeHistorical := owner.calls.Load()
	execution, err := repository.BeginApprovalExecution(ctx, historicalID, policy.ExecutorIdentity, "historical-retry", json.RawMessage(`{}`), json.RawMessage(`{}`))
	if err != nil || execution.Status != "EXECUTING" || execution.FirstClaim || owner.calls.Load() != beforeHistorical {
		t.Fatalf("historical execution read depended on current activation: %+v err=%v calls=%d", execution, err, owner.calls.Load()-beforeHistorical)
	}

	var decisions, outbox, executions int
	if err := repository.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM approval_decisions WHERE approval_request_id=$1),
		(SELECT count(*) FROM outbox_events WHERE aggregate_id=$1),
		(SELECT count(*) FROM approval_executions WHERE approval_request_id=$1)`, approveID).Scan(&decisions, &outbox, &executions); err != nil || decisions != 0 || outbox != 0 || executions != 0 {
		t.Fatalf("inactive approval created effects: %d/%d/%d err=%v", decisions, outbox, executions, err)
	}

	owner.setSnapshot(billingOwnerSnapshot("ACTIVE"))
	activeID := seedAdvancementRequest(t, ctx, repository, requester.ID, policy, "DRAFT")
	if item, err := repository.SubmitApprovalRequest(ctx, requesterPrincipal, activeID, 1); err != nil || item.Status != "SUBMITTED" {
		t.Fatalf("compatible active Billing command regressed: %s %v", item.Status, err)
	}

	revoked := seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PLATFORM_SUPER_ADMIN", "billing-activation-revoked")
	revokedID := seedAdvancementRequest(t, ctx, repository, revoked.ID, policy, "DRAFT")
	if _, err := repository.pool.Exec(ctx, `UPDATE role_assignments SET revoked_at=now() WHERE user_id=$1 AND revoked_at IS NULL`, revoked.ID); err != nil {
		t.Fatal(err)
	}
	beforeRevokedSubmit := owner.calls.Load()
	if _, err := repository.SubmitApprovalRequest(ctx, approvalIntegrationPrincipal(revoked), revokedID, 1); !errors.Is(err, ErrApprovalInvalidated) {
		t.Fatalf("revoked submit did not fail at frozen authorization: %v", err)
	}
	if owner.calls.Load() != beforeRevokedSubmit {
		t.Fatal("revoked submit reached Billing activation before frozen authorization validation")
	}
	assertApprovalRequestState(t, ctx, repository, revokedID, "DRAFT", 1)

	staleAuthorizationID := seedAdvancementRequest(t, ctx, repository, requester.ID, policy, "DRAFT")
	command, err := repository.pool.Exec(ctx, `UPDATE authorization_versions SET version=version+1,updated_at=now() WHERE user_id=$1 AND scope='PLATFORM'`, requester.ID)
	if err != nil || command.RowsAffected() != 1 {
		t.Fatalf("advance authorization version: rows=%d err=%v", command.RowsAffected(), err)
	}
	beforeStaleAuthorization := owner.calls.Load()
	if _, err := repository.SubmitApprovalRequest(ctx, requesterPrincipal, staleAuthorizationID, 1); !errors.Is(err, ErrApprovalInvalidated) {
		t.Fatalf("stale authorization binding submit was not invalidated: %v", err)
	}
	if owner.calls.Load() != beforeStaleAuthorization {
		t.Fatal("stale authorization binding reached Billing activation")
	}
	assertApprovalRequestState(t, ctx, repository, staleAuthorizationID, "DRAFT", 1)

	wrongSnapshotID := seedAdvancementRequest(t, ctx, repository, requester.ID, policy, "DRAFT")
	if _, err := repository.pool.Exec(ctx, `UPDATE approval_request_snapshots SET snapshot_schema_version=2 WHERE approval_request_id=$1`, wrongSnapshotID); err != nil {
		t.Fatal(err)
	}
	beforeWrongSnapshot := owner.calls.Load()
	if _, err := repository.SubmitApprovalRequest(ctx, requesterPrincipal, wrongSnapshotID, 1); !errors.Is(err, ErrApprovalInvalidated) {
		t.Fatalf("wrong frozen snapshot schema submit was not invalidated: %v", err)
	}
	if owner.calls.Load() != beforeWrongSnapshot {
		t.Fatal("wrong frozen snapshot schema reached Billing activation")
	}
	assertApprovalRequestState(t, ctx, repository, wrongSnapshotID, "DRAFT", 1)

	legacyProvider, _ := approvalpolicy.Lookup("PAYMENT_PROVIDER_RESUME")
	legacyProvider.PolicyVersion = 1
	legacyID := seedAdvancementRequest(t, ctx, repository, requester.ID, legacyProvider, "DRAFT")
	legacyUpdate := approvalTestDraft()
	legacyUpdate.CommandType = "PAYMENT_PROVIDER_RESUME"
	legacyUpdate.IdempotencyKey = "legacy-provider-update"
	if _, err := repository.UpdateApprovalDraft(ctx, requesterPrincipal, legacyID, 1, legacyUpdate); !errors.Is(err, ErrApprovalInvalidated) {
		t.Fatalf("legacy Provider p1 update used p2 activation: %v", err)
	}
	if _, err := repository.SubmitApprovalRequest(ctx, requesterPrincipal, legacyID, 1); !errors.Is(err, ErrApprovalInvalidated) {
		t.Fatalf("legacy Provider p1 used p2 activation: %v", err)
	}
	assertApprovalRequestState(t, ctx, repository, legacyID, "DRAFT", 1)

	repository.WithBillingGovernanceActivation(&billingAdvancementReader{snapshot: billingAdvancementSnapshot("ACTIVE"), err: errors.New("transport unavailable")})
	unavailableID := seedAdvancementRequest(t, ctx, repository, requester.ID, policy, "DRAFT")
	if _, err := repository.SubmitApprovalRequest(ctx, requesterPrincipal, unavailableID, 1); !errors.Is(err, ErrBillingGovernanceUnavailable) {
		t.Fatalf("valid stale body plus transport error did not fail closed: %v", err)
	}
	assertApprovalRequestState(t, ctx, repository, unavailableID, "DRAFT", 1)
}

func assertApprovalRequestAbsent(t *testing.T, ctx context.Context, repository *PostgresRepository, requester uuid.UUID, key string) {
	t.Helper()
	var count int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM approval_requests WHERE requester_user_id=$1 AND idempotency_key=$2`, requester, key).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed create persisted approval request: count=%d err=%v", count, err)
	}
}

func assertApprovalRequestState(t *testing.T, ctx context.Context, repository *PostgresRepository, id uuid.UUID, status string, version int64) {
	t.Helper()
	var actualStatus string
	var actualVersion int64
	if err := repository.pool.QueryRow(ctx, `SELECT status,version FROM approval_requests WHERE id=$1`, id).Scan(&actualStatus, &actualVersion); err != nil || actualStatus != status || actualVersion != version {
		t.Fatalf("request state changed: %s/%d want %s/%d err=%v", actualStatus, actualVersion, status, version, err)
	}
}

func seedExecutingBillingApproval(t *testing.T, ctx context.Context, repository *PostgresRepository, requester, approver uuid.UUID, policy approvalpolicy.Policy) uuid.UUID {
	t.Helper()
	id := seedHistoricalAdvancementApproval(t, ctx, repository, requester, approver, policy)
	if _, err := repository.pool.Exec(ctx, `UPDATE approval_requests SET status='EXECUTING',version=3 WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `INSERT INTO approval_executions
		(id,approval_request_id,command_type,command_fingerprint,execution_idempotency_key,executor_identity,job_run_id,status,authorization_snapshot,critical_resource_versions,financial_snapshot,started_at)
		VALUES($1,$2,$3,decode(repeat('ab',32),'hex'),$4,$5,'historical-job','EXECUTING','{}','{}','{}',now())`, uuid.New(), id, policy.CommandType, id.String(), policy.ExecutorIdentity); err != nil {
		t.Fatal(err)
	}
	return id
}
