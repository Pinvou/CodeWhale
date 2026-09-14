package account

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/google/uuid"
)

// Seeds historical rows directly: current draft creation correctly refuses
// policy-only commands, but rows predating that guard must still be contained.
func seedAdvancementRequest(t *testing.T, ctx context.Context, repository *PostgresRepository, requester uuid.UUID, policy approvalpolicy.Policy, status string) uuid.UUID {
	t.Helper()
	binding, version, _, err := strongPlatformAuthorization(ctx, repository.pool, requester, policy.RequesterPermission)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	now := time.Now().UTC()
	var submitted, deadline *time.Time
	if status != "DRAFT" {
		submitted = &now
	}
	if status == "APPROVED" || status == "EXECUTING" || status == "EXECUTED" || status == "EXECUTION_FAILED" {
		value := now.Add(5 * time.Minute)
		deadline = &value
	}
	_, err = repository.pool.Exec(ctx, `INSERT INTO approval_requests
		(id,command_type,policy_version,requester_user_id,requester_binding_id,requester_authorization_version,
		idempotency_key,normalized_command,command_fingerprint,target_resource_type,target_resource_id,reason,
		redacted_evidence,expected_impact,status,version,expires_at,submitted_at,execution_deadline,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,'{}',decode(repeat('ab',32),'hex'),'TEST',$7,'historical approval containment',
		'[]','{}',$8,1,$9,$10,$11,$12,$12)`, id, policy.CommandType, policy.PolicyVersion, requester, binding, version,
		id.String(), status, now.Add(time.Hour), submitted, deadline, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = repository.pool.Exec(ctx, `INSERT INTO approval_request_snapshots
		(approval_request_id,snapshot_schema_version,critical_resource_versions,financial_snapshot,policy_snapshot,requester_authorization_snapshot,created_at)
		VALUES($1,$2,'{}','{}','{}','{}',$3)`, id, policy.SnapshotSchemaVersion, now)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestApprovalAdvancementPostgresIntegration(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	if dsn == "" {
		t.Skip("PINVOU_TEST_ACCOUNT_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	repository, _, _ := newTokenOwnerCandidateIntegrationRepository(t, ctx, dsn)
	requester := seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PLATFORM_SUPER_ADMIN", "advance-requester")
	approver := seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PLATFORM_SUPER_ADMIN", "advance-approver")
	outsider := seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PERSONAL_USER", "advance-outsider")
	requesterPrincipal, approverPrincipal := approvalIntegrationPrincipal(requester), approvalIntegrationPrincipal(approver)
	reader := &advancementActivationReader{state: "SAFE_ROLLBACK"}
	repository.WithTokenGovernanceActivation(reader).WithBillingGovernanceActivation(&billingAdvancementReader{snapshot: billingAdvancementSnapshot("ACTIVE")})
	for _, policy := range approvalpolicy.All() {
		if policy.Mode != approvalpolicy.ApprovalRequired {
			continue
		}
		t.Run(policy.CommandType, func(t *testing.T) {
			for _, state := range []string{"PRE_ACTIVATION", "SAFE_ROLLBACK"} {
				reader.state = state
				if approvalpolicy.TokenExecutableApproval(policy.CommandType) {
					draftID := seedAdvancementRequest(t, ctx, repository, requester.ID, policy, "DRAFT")
					submittedID := seedAdvancementRequest(t, ctx, repository, requester.ID, policy, "SUBMITTED")
					approvedID := seedAdvancementRequest(t, ctx, repository, requester.ID, policy, "APPROVED")
					detail, err := repository.ApprovalRequest(ctx, requesterPrincipal, draftID)
					if err != nil || len(detail.AllowedActions) != 0 {
						t.Fatalf("inactive draft detail actions: %v %v", detail.AllowedActions, err)
					}
					beforeList := reader.calls
					items, err := repository.ListApprovalRequests(ctx, approverPrincipal, ApprovalFilter{CommandType: policy.CommandType, Status: "SUBMITTED"})
					if err != nil || len(items) == 0 || reader.calls-beforeList != 1 {
						t.Fatalf("inactive list observation: items=%d calls=%d error=%v", len(items), reader.calls-beforeList, err)
					}
					for _, item := range items {
						if !slices.Equal(item.AllowedActions, []string{"REJECT"}) {
							t.Fatalf("inactive list actions: %v", item.AllowedActions)
						}
					}
					if _, err := repository.SubmitApprovalRequest(ctx, requesterPrincipal, draftID, 1); !errors.Is(err, ErrTokenGovernanceInactive) {
						t.Fatalf("%s Token submit: %v", state, err)
					}
					if _, err := repository.DecideApprovalRequest(ctx, approverPrincipal, submittedID, 1, "APPROVED", "inactive approval", json.RawMessage(`{}`)); !errors.Is(err, ErrTokenGovernanceInactive) {
						t.Fatalf("%s Token approve: %v", state, err)
					}
					if _, err := repository.BeginApprovalExecution(ctx, approvedID, policy.ExecutorIdentity, "inactive-job", json.RawMessage(`{}`), json.RawMessage(`{}`)); !errors.Is(err, ErrTokenGovernanceInactive) {
						t.Fatalf("%s Token begin: %v", state, err)
					}
					for id, expected := range map[uuid.UUID]string{draftID: "DRAFT", submittedID: "SUBMITTED", approvedID: "APPROVED"} {
						var status string
						var version int64
						if err := repository.pool.QueryRow(ctx, `SELECT status,version FROM approval_requests WHERE id=$1`, id).Scan(&status, &version); err != nil || status != expected || version != 1 {
							t.Fatalf("inactive advancement mutated request: %s/%d %v", status, version, err)
						}
					}
				}
				before := reader.calls
				rejectedID := seedAdvancementRequest(t, ctx, repository, requester.ID, policy, "SUBMITTED")
				if _, err := repository.DecideApprovalRequest(ctx, requesterPrincipal, rejectedID, 1, "REJECTED", "self rejection forbidden", nil); !errors.Is(err, ErrApprovalSelfDecision) {
					t.Fatalf("self decision: %v", err)
				}
				if _, err := repository.DecideApprovalRequest(ctx, approvalIntegrationPrincipal(outsider), rejectedID, 1, "REJECTED", "unauthorized rejection", nil); !errors.Is(err, ErrForbidden) {
					t.Fatalf("outsider decision: %v", err)
				}
				item, err := repository.DecideApprovalRequest(ctx, approverPrincipal, rejectedID, 1, "REJECTED", "safe rejection", nil)
				if err != nil || item.Status != "REJECTED" {
					t.Fatalf("%s reject: %s %v", state, item.Status, err)
				}
				withdrawnID := seedAdvancementRequest(t, ctx, repository, requester.ID, policy, "SUBMITTED")
				item, err = repository.WithdrawApprovalRequest(ctx, requesterPrincipal, withdrawnID, 1)
				if err != nil || item.Status != "WITHDRAWN" || reader.calls != before {
					t.Fatalf("%s withdraw: %s %v, domain calls=%d", state, item.Status, err, reader.calls-before)
				}
			}
			draftID := seedAdvancementRequest(t, ctx, repository, requester.ID, policy, "DRAFT")
			if _, err := repository.WithdrawApprovalRequest(ctx, requesterPrincipal, draftID, 1); !errors.Is(err, ErrConflict) {
				t.Fatalf("draft withdrawal must remain invalid: %v", err)
			}
			if !approvalpolicy.Executable(policy.CommandType) {
				if _, err := repository.SubmitApprovalRequest(ctx, requesterPrincipal, draftID, 1); !errors.Is(err, ErrApprovalCommandNotExecutable) {
					t.Fatalf("policy-only submit: %v", err)
				}
				submittedID := seedAdvancementRequest(t, ctx, repository, requester.ID, policy, "SUBMITTED")
				if _, err := repository.DecideApprovalRequest(ctx, approverPrincipal, submittedID, 1, "APPROVED", "must not authorize", json.RawMessage(`{}`)); !errors.Is(err, ErrApprovalCommandNotExecutable) {
					t.Fatalf("policy-only approve: %v", err)
				}
				approvedID := seedAdvancementRequest(t, ctx, repository, requester.ID, policy, "APPROVED")
				if _, err := repository.BeginApprovalExecution(ctx, approvedID, policy.ExecutorIdentity, "historical-job", json.RawMessage(`{}`), json.RawMessage(`{}`)); !errors.Is(err, ErrApprovalCommandNotExecutable) {
					t.Fatalf("policy-only begin: %v", err)
				}
			}
		})
	}
	policy, _ := approvalpolicy.Lookup("TOKEN_CREDENTIAL_REVOKE")
	revoked := seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PLATFORM_SUPER_ADMIN", "advance-revoked")
	revokedID := seedAdvancementRequest(t, ctx, repository, revoked.ID, policy, "SUBMITTED")
	if _, err := repository.pool.Exec(ctx, `UPDATE role_assignments SET revoked_at=now() WHERE user_id=$1`, revoked.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.WithdrawApprovalRequest(ctx, approvalIntegrationPrincipal(revoked), revokedID, 1); !errors.Is(err, ErrForbidden) {
		t.Fatalf("revoked requester must not withdraw: %v", err)
	}
	var revokedStatus string
	var revokedVersion int64
	if err := repository.pool.QueryRow(ctx, `SELECT status,version FROM approval_requests WHERE id=$1`, revokedID).Scan(&revokedStatus, &revokedVersion); err != nil {
		t.Fatal(err)
	}
	if revokedStatus != "SUBMITTED" || revokedVersion != 1 {
		t.Fatalf("revoked withdrawal mutated request: %s/%d", revokedStatus, revokedVersion)
	}
	raceID := seedAdvancementRequest(t, ctx, repository, requester.ID, policy, "SUBMITTED")
	var wg sync.WaitGroup
	results := make(chan error, 2)
	start := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, err := repository.WithdrawApprovalRequest(ctx, requesterPrincipal, raceID, 1)
		results <- err
	}()
	go func() {
		defer wg.Done()
		<-start
		_, err := repository.DecideApprovalRequest(ctx, approverPrincipal, raceID, 1, "REJECTED", "concurrent rejection", nil)
		results <- err
	}()
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("exactly one safe transition must commit, got %d", successes)
	}
	var executions, events, approvals int
	if err := repository.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM approval_executions),
		(SELECT count(*) FROM outbox_events WHERE event_type='account.approval-command.authorized.v1'),
		(SELECT count(*) FROM approval_decisions WHERE decision='APPROVED')`).Scan(&executions, &events, &approvals); err != nil {
		t.Fatal(err)
	}
	if executions != 0 || events != 0 || approvals != 0 {
		t.Fatalf("unexpected execution authorization: %d/%d/%d", executions, events, approvals)
	}
	// Historical execution retrieval must remain read-only even for commands
	// whose implementation is no longer available. Seed after zero-write checks.
	for _, policy := range approvalpolicy.All() {
		if policy.Mode != approvalpolicy.ApprovalRequired {
			continue
		}
		binding, version, _, err := strongPlatformAuthorization(ctx, repository.pool, approver.ID, policy.ApproverPermission)
		if err != nil {
			t.Fatal(err)
		}
		for _, status := range []string{"EXECUTING", "EXECUTED"} {
			id := seedAdvancementRequest(t, ctx, repository, requester.ID, policy, "SUBMITTED")
			_, err := repository.pool.Exec(ctx, `INSERT INTO approval_decisions
				(id,approval_request_id,approver_user_id,approver_binding_id,approver_authorization_version,decision,reason,authorization_snapshot,decided_at)
				VALUES($1,$2,$3,$4,$5,'APPROVED','historical decision','{}',now())`, uuid.New(), id, approver.ID, binding, version)
			if err != nil {
				t.Fatal(err)
			}
			states := []string{"APPROVED", "EXECUTING"}
			if status == "EXECUTED" {
				states = append(states, "EXECUTED")
			}
			for _, next := range states {
				if _, err := repository.pool.Exec(ctx, `UPDATE approval_requests SET status=$2,execution_deadline=now()+interval '5 minutes',version=version+1 WHERE id=$1`, id, next); err != nil {
					t.Fatal(err)
				}
			}
			_, err = repository.pool.Exec(ctx, `INSERT INTO approval_executions
				(id,approval_request_id,command_type,command_fingerprint,execution_idempotency_key,executor_identity,job_run_id,status,authorization_snapshot,critical_resource_versions,financial_snapshot,started_at)
				VALUES($1,$2,$3,decode(repeat('ab',32),'hex'),$4,$5,'historical-job',$6,'{}','{}','{}',now())`, uuid.New(), id, policy.CommandType, id.String(), policy.ExecutorIdentity, status)
			if err != nil {
				t.Fatal(err)
			}
			before := reader.calls
			lease, err := repository.BeginApprovalExecution(ctx, id, policy.ExecutorIdentity, "retry-job", json.RawMessage(`{}`), json.RawMessage(`{}`))
			if err != nil || lease.Status != status || lease.FirstClaim || reader.calls != before {
				t.Fatalf("historical %s/%s read: %+v %v", policy.CommandType, status, lease, err)
			}
			if _, err := repository.BeginApprovalExecution(ctx, id, "wrong-executor", "retry-job", nil, nil); !errors.Is(err, ErrForbidden) {
				t.Fatalf("historical execution identity: %v", err)
			}
			var storedStatus, storedJob string
			var storedVersion int64
			if err := repository.pool.QueryRow(ctx, `SELECT a.status,a.version,e.job_run_id FROM approval_requests a JOIN approval_executions e ON e.approval_request_id=a.id WHERE a.id=$1`, id).Scan(&storedStatus, &storedVersion, &storedJob); err != nil || storedStatus != status || storedVersion != int64(1+len(states)) || storedJob != "historical-job" {
				t.Fatalf("historical read changed state: %s/%d/%s %v", storedStatus, storedVersion, storedJob, err)
			}
		}
	}
}
