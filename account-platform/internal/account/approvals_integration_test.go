package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

func approvedAdjustmentForTest(t *testing.T, repo *PostgresRepository) (ApprovalRequest, identity.Principal, identity.Principal) {
	return approvedAdjustmentWithExpiryForTest(t, repo, nil)
}

func TestPostgresApprovalDecisionUsesTimeAfterAuthorityLock(t *testing.T) {
	for _, scenario := range []string{"approved_request_expired", "rejected_request_expired", "approver_expired", "current_decision_clock"} {
		t.Run(scenario, func(t *testing.T) {
			repo := accountDatabase(t)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			requester, approver := fixturePlatformAdmin(t, repo), fixturePlatformAdmin(t, repo)
			requester.AuthenticatedAt, approver.AuthenticatedAt = time.Now(), time.Now()
			requester.SessionID, approver.SessionID = "requester-interactive", "approver-interactive"
			draft := approvalTestDraft()
			created, err := repo.CreateApprovalDraft(ctx, requester, draft)
			if err != nil {
				t.Fatal(err)
			}
			submitted, err := repo.SubmitApprovalRequest(ctx, requester, created.ID, created.Version)
			if err != nil {
				t.Fatal(err)
			}
			var boundary time.Time
			if scenario == "approver_expired" {
				err = repo.pool.QueryRow(ctx, `UPDATE role_assignments SET expires_at=clock_timestamp()+interval '1 second' WHERE user_id=(SELECT user_id FROM identities WHERE issuer=$1 AND subject=$2) RETURNING expires_at`, approver.Issuer, approver.Subject).Scan(&boundary)
			} else {
				err = repo.pool.QueryRow(ctx, `UPDATE approval_requests SET expires_at=clock_timestamp()+interval '1 second' WHERE id=$1 RETURNING expires_at`, submitted.ID).Scan(&boundary)
			}
			if err != nil {
				t.Fatal(err)
			}
			gate, err := repo.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer gate.Rollback(context.Background())
			var blocker int32
			if err := gate.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blocker); err != nil {
				t.Fatal(err)
			}
			if _, err := gate.Exec(ctx, `SELECT ra.id FROM role_assignments ra JOIN identities i ON i.user_id=ra.user_id WHERE i.issuer=$1 AND i.subject=$2 FOR UPDATE OF ra`, approver.Issuer, approver.Subject); err != nil {
				t.Fatal(err)
			}
			decision := "APPROVED"
			if scenario == "rejected_request_expired" {
				decision = "REJECTED"
			}
			type result struct {
				item ApprovalRequest
				err  error
			}
			done := make(chan result, 1)
			go func() {
				item, err := repo.DecideApprovalRequest(ctx, approver, submitted.ID, submitted.Version, decision, "independent repair", draft.CriticalResourceVersions)
				done <- result{item, err}
			}()
			for {
				var waiting bool
				if err := repo.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				select {
				case <-done:
					t.Fatal("decision did not wait for the controlled authority lock")
				default:
				}
				if time.Now().After(boundary) {
					t.Fatal("controlled decision did not reach the lock wait before expiry")
				}
				time.Sleep(5 * time.Millisecond)
			}
			if scenario == "current_decision_clock" {
				time.Sleep(50 * time.Millisecond)
			} else {
				time.Sleep(time.Until(boundary) + 20*time.Millisecond)
			}
			var releasedAt time.Time
			if err := gate.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&releasedAt); err != nil {
				t.Fatal(err)
			}
			if err := gate.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			got := <-done
			var status string
			var decisions, events int
			if err := repo.pool.QueryRow(ctx, `SELECT status,(SELECT count(*) FROM approval_decisions WHERE approval_request_id=$1),(SELECT count(*) FROM outbox_events WHERE aggregate_id=$1 AND event_type='account.approval-command.authorized.v1') FROM approval_requests WHERE id=$1`, submitted.ID).Scan(&status, &decisions, &events); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "approver_expired":
				if !errors.Is(got.err, ErrForbidden) || status != "SUBMITTED" || decisions != 0 || events != 0 {
					t.Fatal("expired approver produced a decision or authorization event")
				}
			case "current_decision_clock":
				policy, err := approvalpolicy.Lookup(got.item.CommandType)
				if err != nil || got.err != nil || status != "APPROVED" || decisions != 1 || events != 1 || got.item.DecidedAt == nil || got.item.DecidedAt.Before(releasedAt) || got.item.ExecutionDeadline == nil || !got.item.ExecutionDeadline.Equal(got.item.DecidedAt.Add(policy.ExecutionWindow)) {
					t.Fatal("decision and execution window did not use the post-lock database clock")
				}
			default:
				if !errors.Is(got.err, ErrApprovalExpired) || status != "EXPIRED" || decisions != 0 || events != 0 {
					t.Fatal("request expired during the lock wait produced a decision or authorization event")
				}
			}
		})
	}
}

func approvedAdjustmentWithExpiryForTest(t *testing.T, repo *PostgresRepository, expiry *time.Time) (ApprovalRequest, identity.Principal, identity.Principal) {
	t.Helper()
	ctx := context.Background()
	requester, approver := fixturePlatformAdmin(t, repo), fixturePlatformAdmin(t, repo)
	requester.AuthenticatedAt, approver.AuthenticatedAt = time.Now(), time.Now()
	requester.SessionID, approver.SessionID = "requester-interactive", "approver-interactive"
	if _, err := repo.pool.Exec(ctx, `UPDATE role_assignments SET role_code='PLATFORM_BILLING_OPERATOR' WHERE user_id=(SELECT user_id FROM identities WHERE issuer=$1 AND subject=$2)`, requester.Issuer, requester.Subject); err != nil {
		t.Fatal(err)
	}
	if expiry != nil {
		if _, err := repo.pool.Exec(ctx, `UPDATE role_assignments SET expires_at=$3 WHERE user_id=(SELECT user_id FROM identities WHERE issuer=$1 AND subject=$2)`, requester.Issuer, requester.Subject, *expiry); err != nil {
			t.Fatal(err)
		}
	}
	draft := approvalTestDraft()
	draft.IdempotencyKey = uuid.NewString()
	created, err := repo.CreateApprovalDraft(ctx, requester, draft)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := repo.CreateApprovalDraft(ctx, requester, draft)
	if err != nil || replay.ID != created.ID {
		t.Fatal("draft retry did not preserve its frozen identity")
	}
	submitted, err := repo.SubmitApprovalRequest(ctx, requester, created.ID, created.Version)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.DecideApprovalRequest(ctx, requester, submitted.ID, submitted.Version, "APPROVED", "independent repair", draft.CriticalResourceVersions); !errors.Is(err, ErrApprovalSelfDecision) {
		t.Fatal("requester approved their own financial command")
	}
	approved, err := repo.DecideApprovalRequest(ctx, approver, submitted.ID, submitted.Version, "APPROVED", "independent repair", draft.CriticalResourceVersions)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := repo.pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id=$1 AND event_type='account.approval-command.authorized.v1'`, approved.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("approval did not atomically publish exactly one authorization event")
	}
	return approved, requester, approver
}

func TestPostgresApprovalExecutionFrozenReplayAndCompletionFacts(t *testing.T) {
	repo := accountDatabase(t)
	ctx := context.Background()
	approved, _, _ := approvedAdjustmentForTest(t, repo)
	first, err := repo.BeginApprovalExecution(ctx, approved.ID, "billing-command-executor", "same-event-id", approved.CriticalResourceVersions)
	if err != nil {
		t.Fatal(err)
	}
	// After the atomic EXECUTING transition, later revocation cannot erase an
	// unknown financial outcome. Replay must retain the frozen authority.
	if _, err := repo.pool.Exec(ctx, `UPDATE role_assignments SET revoked_at=statement_timestamp() WHERE user_id=$1`, approved.RequesterUserID); err != nil {
		t.Fatal(err)
	}
	replay, err := repo.BeginApprovalExecution(ctx, approved.ID, "billing-command-executor", "same-event-id", json.RawMessage(`{"adjustment_version":2}`))
	if err != nil || replay.ExecutionIdempotencyKey != first.ExecutionIdempotencyKey || replay.CommandFingerprint != first.CommandFingerprint {
		t.Fatal("frozen execution could not resume after financial progress or role revocation")
	}
	if _, err := repo.BeginApprovalExecution(ctx, approved.ID, "billing-command-executor", "different-event-id", approved.CriticalResourceVersions); !errors.Is(err, ErrConflict) {
		t.Fatal("different job identity reused a frozen execution")
	}
	domain, ledger := json.RawMessage(`{"status":"POSTED"}`), json.RawMessage(`{"journal_entry_id":"synthetic-journal"}`)
	for i := 0; i < 2; i++ {
		if err := repo.CompleteApprovalExecution(ctx, approved.ID, "billing-command-executor", "same-event-id", "EXECUTED", domain, ledger, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.CompleteApprovalExecution(ctx, approved.ID, "billing-command-executor", "same-event-id", "EXECUTED", json.RawMessage(`{"status":"DIFFERENT"}`), ledger, ""); !errors.Is(err, ErrConflict) {
		t.Fatal("completion replay accepted different domain facts")
	}
	var status string
	var completionCount int
	if err := repo.pool.QueryRow(ctx, `SELECT status FROM approval_requests WHERE id=$1`, approved.ID).Scan(&status); err != nil || status != "EXECUTED" {
		t.Fatal("completion did not persist the authoritative final state")
	}
	if err := repo.pool.QueryRow(ctx, `SELECT count(*) FROM approval_state_events WHERE approval_request_id=$1 AND reason_code='EXECUTED'`, approved.ID).Scan(&completionCount); err != nil || completionCount != 1 {
		t.Fatal("completion retry duplicated final audit events")
	}
}

func TestPostgresApprovalFirstExecutionAndDisableHaveOneAuthorityOrder(t *testing.T) {
	for _, mutation := range []string{"disable", "revoke"} {
		t.Run(mutation, func(t *testing.T) { exerciseApprovalAuthorityOrder(t, mutation) })
	}
}

func exerciseApprovalAuthorityOrder(t *testing.T, mutation string) {
	repo := accountDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	approved, _, approver := approvedAdjustmentForTest(t, repo)
	gate, err := repo.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Release()
	key := time.Now().UnixNano() & 0x7fffffff
	if _, err := gate.Exec(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		t.Fatal(err)
	}
	unlock := func() { _, _ = gate.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, key) }
	defer unlock()
	accountExec(t, repo, fmt.Sprintf(`CREATE FUNCTION pause_approval_first_execution() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(%d); RETURN NEW; END $$; CREATE TRIGGER pause_first_execution BEFORE INSERT ON approval_executions FOR EACH ROW EXECUTE FUNCTION pause_approval_first_execution()`, key))
	beginDone, disableDone := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := repo.BeginApprovalExecution(ctx, approved.ID, "billing-command-executor", "same-event-id", approved.CriticalResourceVersions)
		beginDone <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	var beginPID int32
	for {
		if err := repo.pool.QueryRow(ctx, `SELECT COALESCE((SELECT pid FROM pg_locks WHERE locktype='advisory' AND objid=$1 AND NOT granted LIMIT 1),0)`, key).Scan(&beginPID); err != nil {
			t.Fatal(err)
		}
		if beginPID != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("execution did not reach its controlled insertion gate")
		}
		time.Sleep(5 * time.Millisecond)
	}
	go func() {
		if mutation == "revoke" {
			_, err := repo.AdminRevokeRoleBinding(ctx, approver, approved.RequesterBindingID)
			disableDone <- err
		} else {
			disableDone <- repo.AdminSetUserStatus(ctx, approver, approved.RequesterUserID, "DISABLED")
		}
	}()
	// A disable that commits before EXECUTING must invalidate the first Begin.
	// If Begin has already fenced authority, disable instead waits for its lock.
	disabledFirst := false
	deadline = time.Now().Add(3 * time.Second)
	for {
		select {
		case err := <-disableDone:
			if err != nil {
				t.Fatal(err)
			}
			disabledFirst = true
		default:
		}
		if disabledFirst {
			break
		}
		var waiting bool
		if err := repo.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, beginPID).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("disable neither completed nor waited on fenced authority")
		}
		time.Sleep(5 * time.Millisecond)
	}
	unlock()
	beginErr := <-beginDone
	if disabledFirst && beginErr == nil {
		t.Fatal("first execution authorized after its requester was disabled")
	}
	if !disabledFirst {
		// A serialization rejection is also safe: neither transaction may use a
		// stale authorization snapshot to establish first execution authority.
		if err := <-disableDone; err != nil && beginErr != nil {
			t.Fatal("both concurrent operations failed without a winner")
		}
	}
}

func TestPostgresApprovalFirstExecutionRevalidatesFrozenAuthority(t *testing.T) {
	for _, scenario := range []string{"requester-revoked", "approver-revoked", "resource-changed", "window-expired"} {
		t.Run(scenario, func(t *testing.T) {
			repo := accountDatabase(t)
			ctx := context.Background()
			approved, _, approver := approvedAdjustmentForTest(t, repo)
			observed := approved.CriticalResourceVersions
			switch scenario {
			case "requester-revoked":
				if _, err := repo.pool.Exec(ctx, `UPDATE role_assignments SET revoked_at=statement_timestamp() WHERE user_id=$1`, approved.RequesterUserID); err != nil {
					t.Fatal(err)
				}
			case "approver-revoked":
				// Keep a distinct recovery administrator: revoking the final
				// super administrator is correctly prohibited by production SQL.
				_ = fixturePlatformAdmin(t, repo)
				if _, err := repo.pool.Exec(ctx, `UPDATE role_assignments SET revoked_at=statement_timestamp() WHERE user_id=(SELECT user_id FROM identities WHERE issuer=$1 AND subject=$2)`, approver.Issuer, approver.Subject); err != nil {
					t.Fatal(err)
				}
			case "resource-changed":
				observed = json.RawMessage(`{"adjustment_version":2}`)
			case "window-expired":
				if _, err := repo.pool.Exec(ctx, `UPDATE approval_requests SET execution_deadline=statement_timestamp()-interval '1 second' WHERE id=$1`, approved.ID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := repo.BeginApprovalExecution(ctx, approved.ID, "billing-command-executor", "same-event-id", observed); !errors.Is(err, ErrApprovalInvalidated) {
				t.Fatal("first execution bypassed its current authority or frozen resource facts")
			}
			var count int
			if err := repo.pool.QueryRow(ctx, `SELECT count(*) FROM approval_executions WHERE approval_request_id=$1`, approved.ID).Scan(&count); err != nil || count != 0 {
				t.Fatal("invalid first execution persisted financial authorization")
			}
		})
	}
}

func TestPostgresApprovalRoleExpiryWhileAuthorityLockWaits(t *testing.T) {
	repo := accountDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	expires := time.Now().Add(time.Second)
	approved, _, _ := approvedAdjustmentWithExpiryForTest(t, repo, &expires)
	gate, err := repo.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(context.Background())
	var blocker int32
	if err := gate.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blocker); err != nil {
		t.Fatal(err)
	}
	// Expiry was included in the approved authorization snapshot. No version
	// mutation can mask the time boundary while this lock is held.
	if _, err := gate.Exec(ctx, `SELECT id FROM role_assignments WHERE id=$1 FOR UPDATE`, approved.RequesterBindingID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := repo.BeginApprovalExecution(ctx, approved.ID, "billing-command-executor", "same-event-id", approved.CriticalResourceVersions)
		done <- err
	}()
	for {
		var waiting bool
		if err := repo.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("execution did not wait for its authority lock: %v", err)
		default:
		}
		if time.Now().After(expires) {
			t.Fatal("expiry fixture failed to reach the lock wait before expiry")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(time.Until(expires) + 20*time.Millisecond)
	if err := gate.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrApprovalInvalidated) {
		t.Fatal("expired role authorized first execution after waiting on a lock")
	}
}

func TestPostgresApprovalExecutionWindowExpiresWhileAuthorityLockWaits(t *testing.T) {
	repo := accountDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	approved, _, _ := approvedAdjustmentForTest(t, repo)
	var expires time.Time
	if err := repo.pool.QueryRow(ctx, `UPDATE approval_requests SET execution_deadline=clock_timestamp()+interval '1 second' WHERE id=$1 RETURNING execution_deadline`, approved.ID).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	gate, err := repo.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(context.Background())
	var blocker int32
	if err := gate.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blocker); err != nil {
		t.Fatal(err)
	}
	if _, err := gate.Exec(ctx, `SELECT id FROM role_assignments WHERE id=$1 FOR UPDATE`, approved.RequesterBindingID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := repo.BeginApprovalExecution(ctx, approved.ID, "billing-command-executor", "same-event-id", approved.CriticalResourceVersions)
		done <- err
	}()
	for {
		var waiting bool
		if err := repo.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("execution did not wait for its authority lock: %v", err)
		default:
		}
		if time.Now().After(expires) {
			t.Fatal("execution window expired before the controlled lock wait")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(time.Until(expires) + 20*time.Millisecond)
	if err := gate.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrApprovalInvalidated) {
		t.Fatal("expired execution window authorized first execution after its authority lock wait")
	}
	var status, code string
	var executions int
	if err := repo.pool.QueryRow(ctx, `SELECT status,COALESCE(invalidation_code,''),(SELECT count(*) FROM approval_executions WHERE approval_request_id=$1) FROM approval_requests WHERE id=$1`, approved.ID).Scan(&status, &code, &executions); err != nil || status != "INVALIDATED" || code != "EXECUTION_WINDOW_EXPIRED" || executions != 0 {
		t.Fatal("expired first execution did not persist invalidation without execution authority")
	}
}

func TestPostgresApprovalRequesterExpiryWhileApproverLockWaits(t *testing.T) {
	repo := accountDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	expires := time.Now().Add(time.Second)
	approved, _, approver := approvedAdjustmentWithExpiryForTest(t, repo, &expires)
	gate, err := repo.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(context.Background())
	var blocker int32
	if err := gate.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blocker); err != nil {
		t.Fatal(err)
	}
	// Requester authority is acquired first and remains unmutated. Waiting
	// for the independent approver must not extend that role lifetime.
	if _, err := gate.Exec(ctx, `SELECT id FROM role_assignments WHERE user_id=(SELECT user_id FROM identities WHERE issuer=$1 AND subject=$2) FOR UPDATE`, approver.Issuer, approver.Subject); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := repo.BeginApprovalExecution(ctx, approved.ID, "billing-command-executor", "same-event-id", approved.CriticalResourceVersions)
		done <- err
	}()
	for {
		var waiting bool
		if err := repo.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("execution did not wait for its authority lock: %v", err)
		default:
		}
		if time.Now().After(expires) {
			t.Fatal("expiry fixture failed to reach the lock wait before expiry")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(time.Until(expires) + 20*time.Millisecond)
	if err := gate.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrApprovalInvalidated) {
		t.Fatal("requester expired while approver lock waited but first execution still authorized")
	}
}
