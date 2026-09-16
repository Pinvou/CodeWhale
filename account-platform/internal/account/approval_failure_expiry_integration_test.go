package account

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
)

func TestFailedApprovalExpiryRetainsUnresolvedExecutionIntegration(t *testing.T) {
	db, repository, relay, _ := newApprovalDeliveryIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	binding := publishedApprovalDeliveryBinding(t, ctx, db, repository, relay, approvalconsumer.LegacyBilling().Durable)
	state := observeApprovalDelivery(t, ctx, repository, binding, 71)
	if _, err := repository.GrantApprovalAuthorizationDomainAttempt(ctx, binding.EventID, binding.Durable, binding.Workload, state.Version); err != nil {
		t.Fatal(err)
	}
	var critical, financial json.RawMessage
	if err := db.adminPool.QueryRow(ctx, `SELECT critical_resource_versions,financial_snapshot FROM public.approval_request_snapshots WHERE approval_request_id=$1`, binding.ApprovalRequestID).Scan(&critical, &financial); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.BeginApprovalExecutionWithAuthorization(ctx, binding.ApprovalRequestID, binding.Workload, binding.EventID.String(), critical, financial, binding); err != nil {
		t.Fatal(err)
	}
	if err := repository.CompleteApprovalExecution(ctx, binding.ApprovalRequestID, binding.Workload, binding.EventID.String(), "FAILED", json.RawMessage(`{}`), json.RawMessage(`{}`), "DOMAIN_PRECONDITION_REJECTED"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.adminPool.Exec(ctx, `UPDATE approval_requests SET execution_deadline=now()-interval '1 second' WHERE id=$1`, binding.ApprovalRequestID); err != nil {
		t.Fatal(err)
	}
	count, err := repository.ExpireApprovalRequests(ctx, 100)
	if err != nil || count != 0 {
		t.Fatalf("expiry consumed unresolved execution: count=%d err=%v", count, err)
	}
	if changed, err := repository.expireApprovalRequest(ctx, binding.ApprovalRequestID); err != nil || changed {
		t.Fatalf("direct expiry invalidated unresolved execution: changed=%v err=%v", changed, err)
	}
	var requestStatus, executionStatus string
	if err := db.adminPool.QueryRow(ctx, `SELECT r.status,e.status FROM approval_requests r JOIN approval_executions e ON e.approval_request_id=r.id WHERE r.id=$1`, binding.ApprovalRequestID).Scan(&requestStatus, &executionStatus); err != nil || requestStatus != "EXECUTION_FAILED" || executionStatus != "FAILED" {
		t.Fatalf("unresolved execution changed: request=%s execution=%s err=%v", requestStatus, executionStatus, err)
	}
}

func TestApprovedWithoutExecutionStillExpiresIntegration(t *testing.T) {
	db, repository, relay, _ := newApprovalDeliveryIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	binding := publishedApprovalDeliveryBinding(t, ctx, db, repository, relay, approvalconsumer.LegacyBilling().Durable)
	if _, err := db.adminPool.Exec(ctx, `UPDATE approval_requests SET execution_deadline=now()-interval '1 second' WHERE id=$1`, binding.ApprovalRequestID); err != nil {
		t.Fatal(err)
	}
	count, err := repository.ExpireApprovalRequests(ctx, 100)
	if err != nil || count != 1 {
		t.Fatalf("unstarted approved request did not expire: count=%d err=%v", count, err)
	}
	var status, code string
	if err := db.adminPool.QueryRow(ctx, `SELECT status,invalidation_code FROM approval_requests WHERE id=$1`, binding.ApprovalRequestID).Scan(&status, &code); err != nil || status != "INVALIDATED" || code != "EXECUTION_WINDOW_EXPIRED" {
		t.Fatalf("approved expiry status=%s code=%s err=%v", status, code, err)
	}
}
