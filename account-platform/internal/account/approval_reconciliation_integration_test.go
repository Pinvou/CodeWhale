package account

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvaleffect"
	"github.com/google/uuid"
)

func TestExpiredApprovalReconciliationPostgresIntegration(t *testing.T) {
	db, repository, _, _ := newApprovalDeliveryIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pendingEvent := seedBackfillAuthorization(t, ctx, db, true)
	deadEvent := seedBackfillAuthorization(t, ctx, db, true)
	effectEvent := seedBackfillAuthorization(t, ctx, db, true)
	executionEvent := seedBackfillAuthorization(t, ctx, db, true)
	events := []uuid.UUID{pendingEvent, deadEvent, effectEvent, executionEvent}
	requestByEvent := make(map[uuid.UUID]uuid.UUID, len(events))
	for _, eventID := range events {
		var requestID uuid.UUID
		if err := db.adminPool.QueryRow(ctx, `SELECT aggregate_id FROM public.outbox_events WHERE event_id=$1`, eventID).Scan(&requestID); err != nil {
			t.Fatal(err)
		}
		requestByEvent[eventID] = requestID
		if _, err := db.adminPool.Exec(ctx, `UPDATE public.approval_requests
SET execution_deadline=transaction_timestamp()-interval '1 minute',updated_at=transaction_timestamp()
WHERE id=$1 AND status='APPROVED'`, requestID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.adminPool.Exec(ctx, `ALTER TABLE public.outbox_events DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.adminPool.Exec(ctx, `UPDATE public.outbox_events SET attempts=12,dead_lettered_at=transaction_timestamp(),last_error='EVENT_DELIVERY_FAILED' WHERE event_id=$1`, deadEvent); err != nil {
		t.Fatal(err)
	}
	if _, err := db.adminPool.Exec(ctx, `ALTER TABLE public.outbox_events ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.adminPool.Exec(ctx, `ALTER TABLE public.approval_executions DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.adminPool.Exec(ctx, `INSERT INTO public.approval_executions(id,approval_request_id,command_type,command_fingerprint,
execution_idempotency_key,executor_identity,job_run_id,status,authorization_snapshot,critical_resource_versions,financial_snapshot,started_at)
SELECT $1,r.id,r.command_type,r.command_fingerprint,'fixture:begin-won','billing-command-executor','fixture-job','EXECUTING','{}',
s.critical_resource_versions,s.financial_snapshot,transaction_timestamp() FROM public.approval_requests r
JOIN public.approval_request_snapshots s ON s.approval_request_id=r.id WHERE r.id=$2`, uuid.Must(uuid.NewV7()), requestByEvent[executionEvent]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.adminPool.Exec(ctx, `ALTER TABLE public.approval_executions ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	setApprovalConsumerControl(t, ctx, db, "QUIESCING", "RUNNING", 1, false, 2)
	plan, err := repository.PlanApprovalReconciliation(ctx, 20)
	if err != nil || len(plan.Candidates) != 4 || plan.MoreCandidates {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	var pendingBefore []byte
	if err := db.adminPool.QueryRow(ctx, `SELECT to_jsonb(o)::text FROM public.outbox_events o WHERE event_id=$1`, pendingEvent).Scan(&pendingBefore); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range plan.Candidates {
		state := approvaleffect.StateAbsent
		if candidate.RequestID == requestByEvent[effectEvent] {
			state = approvaleffect.StateObserved
		}
		observation := approvaleffect.Observation{SchemaVersion: 1, Authority: approvaleffect.AuthorityBilling,
			ApprovalRequestID: candidate.RequestID, CommandType: candidate.CommandType, CommandFingerprint: candidate.CommandFingerprint,
			State: state, ObservedAt: time.Now().UTC()}
		result, err := repository.ReconcileApproval(ctx, plan.Control, candidate, observation, true)
		if err != nil {
			t.Fatal(err)
		}
		switch candidate.RequestID {
		case requestByEvent[pendingEvent]:
			if result != ApprovalReconciliationInvalidated {
				t.Fatalf("pending result=%s", result)
			}
		case requestByEvent[deadEvent]:
			if result != ApprovalReconciliationExclusionSealed {
				t.Fatalf("dead result=%s", result)
			}
		case requestByEvent[effectEvent]:
			if result != ApprovalReconciliationBlockedEffect {
				t.Fatalf("effect result=%s", result)
			}
		case requestByEvent[executionEvent]:
			if result != ApprovalReconciliationBlockedExecution {
				t.Fatalf("execution result=%s", result)
			}
		}
	}
	var pendingAfter []byte
	if err := db.adminPool.QueryRow(ctx, `SELECT to_jsonb(o)::text FROM public.outbox_events o WHERE event_id=$1`, pendingEvent).Scan(&pendingAfter); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pendingBefore, pendingAfter) {
		t.Fatal("reconciliation rewrote the original Outbox row")
	}
	for _, eventID := range []uuid.UUID{pendingEvent, deadEvent} {
		var status, code string
		var executions int
		if err := db.adminPool.QueryRow(ctx, `SELECT r.status,r.invalidation_code,
(SELECT count(*) FROM public.approval_executions e WHERE e.approval_request_id=r.id)
FROM public.approval_requests r WHERE r.id=$1`, requestByEvent[eventID]).Scan(&status, &code, &executions); err != nil {
			t.Fatal(err)
		}
		if status != "INVALIDATED" || code != "EXECUTION_WINDOW_EXPIRED" || executions != 0 {
			t.Fatalf("request %s status=%s code=%s executions=%d", requestByEvent[eventID], status, code, executions)
		}
	}
	for _, eventID := range []uuid.UUID{effectEvent, executionEvent} {
		var blockedStatus string
		if err := db.adminPool.QueryRow(ctx, `SELECT status FROM public.approval_requests WHERE id=$1`, requestByEvent[eventID]).Scan(&blockedStatus); err != nil || blockedStatus != "APPROVED" {
			t.Fatalf("blocked request %s status=%s err=%v", requestByEvent[eventID], blockedStatus, err)
		}
	}
	var exclusionCount, auditCount int
	if err := db.adminPool.QueryRow(ctx, `SELECT
(SELECT count(*) FROM account_approval_authorization.approval_authorization_delivery_exclusions WHERE event_id=$1),
(SELECT count(*) FROM public.audit_logs WHERE action='approval.authorization-exclusion.effect-verified' AND resource_id=$1::text)`, deadEvent).Scan(&exclusionCount, &auditCount); err != nil {
		t.Fatal(err)
	}
	if exclusionCount != 1 || auditCount != 1 {
		t.Fatalf("exclusion=%d audit=%d", exclusionCount, auditCount)
	}
	second, err := repository.PlanApprovalReconciliation(ctx, 20)
	if err != nil || len(second.Candidates) != 2 {
		t.Fatalf("idempotent plan=%+v err=%v", second, err)
	}
	changed := plan.Control
	changed.Version++
	var candidate ApprovalReconciliationCandidate
	for _, item := range second.Candidates {
		if item.RequestID == requestByEvent[effectEvent] {
			candidate = item
		}
	}
	if candidate.RequestID == uuid.Nil {
		t.Fatal("effect-blocked candidate disappeared")
	}
	absent := approvaleffect.Observation{SchemaVersion: 1, Authority: approvaleffect.AuthorityBilling, ApprovalRequestID: candidate.RequestID,
		CommandType: candidate.CommandType, CommandFingerprint: candidate.CommandFingerprint, State: approvaleffect.StateAbsent, ObservedAt: time.Now().UTC()}
	if _, err := repository.ReconcileApproval(ctx, changed, candidate, absent, true); !errors.Is(err, ErrApprovalReconciliationControlChanged) {
		t.Fatalf("control drift err=%v", err)
	}
}

func TestExpiredApprovalReconciliationSealsSafeRollbackDeadLetterPostgresIntegration(t *testing.T) {
	db, repository, _, _ := newApprovalDeliveryIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	eventID := seedBackfillAuthorization(t, ctx, db, true)
	var requestID uuid.UUID
	if err := db.adminPool.QueryRow(ctx, `SELECT aggregate_id FROM public.outbox_events WHERE event_id=$1`, eventID).Scan(&requestID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.adminPool.Exec(ctx, `UPDATE public.approval_requests
SET execution_deadline=transaction_timestamp()-interval '1 minute',updated_at=transaction_timestamp()
WHERE id=$1 AND status='APPROVED'`, requestID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.adminPool.Exec(ctx, `ALTER TABLE public.outbox_events DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.adminPool.Exec(ctx, `UPDATE public.outbox_events SET attempts=12,dead_lettered_at=transaction_timestamp(),last_error='EVENT_DELIVERY_FAILED' WHERE event_id=$1`, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.adminPool.Exec(ctx, `ALTER TABLE public.outbox_events ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	setApprovalConsumerControl(t, ctx, db, "SAFE_ROLLBACK", "RUNNING", 1, true, 3)
	plan, err := repository.PlanApprovalReconciliation(ctx, 20)
	if err != nil || len(plan.Candidates) != 1 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	candidate := plan.Candidates[0]
	observation := approvaleffect.Observation{SchemaVersion: 1, Authority: approvaleffect.AuthorityBilling,
		ApprovalRequestID: candidate.RequestID, CommandType: candidate.CommandType, CommandFingerprint: candidate.CommandFingerprint,
		State: approvaleffect.StateAbsent, ObservedAt: time.Now().UTC()}
	result, err := repository.ReconcileApproval(ctx, plan.Control, candidate, observation, true)
	if err != nil || result != ApprovalReconciliationExclusionSealed {
		t.Fatalf("result=%s err=%v", result, err)
	}
	var reason string
	if err := db.adminPool.QueryRow(ctx, `SELECT reason_code FROM account_approval_authorization.approval_authorization_delivery_exclusions WHERE event_id=$1`, eventID).Scan(&reason); err != nil || reason != "SAFE_ROLLBACK_INVALIDATED_NO_EXECUTION" {
		t.Fatalf("reason=%s err=%v", reason, err)
	}
}

func TestSafeRollbackApprovalReconciliationPostgresIntegration(t *testing.T) {
	for _, test := range []struct {
		name         string
		controlEpoch int64
		wantResult   ApprovalReconciliationResult
		wantStatus   string
		wantCode     *string
		wantEligible int
		wantHistory  int
	}{
		{name: "previous epoch is invalidated inside execution window", controlEpoch: 2, wantResult: ApprovalReconciliationInvalidated,
			wantStatus: "INVALIDATED", wantCode: stringPointer("APPROVAL_SAFE_ROLLBACK_INVALIDATED"), wantEligible: 1},
		{name: "older epoch remains a hard history blocker", controlEpoch: 3, wantResult: ApprovalReconciliationBlockedHistory,
			wantStatus: "APPROVED", wantHistory: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, repository, _, _ := newApprovalDeliveryIntegrationDatabase(t)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			eventID := seedBackfillAuthorization(t, ctx, db, true)
			var requestID uuid.UUID
			if err := db.adminPool.QueryRow(ctx, `SELECT aggregate_id FROM public.outbox_events WHERE event_id=$1`, eventID).Scan(&requestID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.adminPool.Exec(ctx, `UPDATE public.approval_requests
SET execution_deadline=transaction_timestamp()+interval '4 minutes',updated_at=transaction_timestamp()
WHERE id=$1 AND status='APPROVED'`, requestID); err != nil {
				t.Fatal(err)
			}
			setApprovalConsumerControl(t, ctx, db, "SAFE_ROLLBACK", "RUNNING", 1, true, test.controlEpoch)
			dryRun, err := RunApprovalReconciliation(ctx, repository, reconciliationEffectStub{}, 20, false)
			if err != nil || dryRun.EligibleCount != test.wantEligible || dryRun.HistoryBlockedCount != test.wantHistory ||
				dryRun.CompleteWithoutBlocker != (test.wantHistory == 0) {
				t.Fatalf("dry-run=%+v err=%v", dryRun, err)
			}
			plan, err := repository.PlanApprovalReconciliation(ctx, 20)
			if err != nil || len(plan.Candidates) != 1 || plan.Candidates[0].InvalidationCode != "APPROVAL_SAFE_ROLLBACK_INVALIDATED" {
				t.Fatalf("plan=%+v err=%v", plan, err)
			}
			candidate := plan.Candidates[0]
			observation := approvaleffect.Observation{SchemaVersion: 1, Authority: approvaleffect.AuthorityBilling,
				ApprovalRequestID: candidate.RequestID, CommandType: candidate.CommandType, CommandFingerprint: candidate.CommandFingerprint,
				State: approvaleffect.StateAbsent, ObservedAt: time.Now().UTC()}
			result, err := repository.ReconcileApproval(ctx, plan.Control, candidate, observation, true)
			if err != nil || result != test.wantResult {
				t.Fatalf("result=%s err=%v", result, err)
			}
			var status string
			var code *string
			if err := db.adminPool.QueryRow(ctx, `SELECT status,invalidation_code FROM public.approval_requests WHERE id=$1`, requestID).Scan(&status, &code); err != nil {
				t.Fatal(err)
			}
			if status != test.wantStatus || !sameOptionalString(code, test.wantCode) {
				t.Fatalf("status=%s code=%v", status, code)
			}
		})
	}
}

func stringPointer(value string) *string { return &value }

func sameOptionalString(left, right *string) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}
