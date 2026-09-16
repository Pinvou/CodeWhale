package account

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/billinggovernance"
	"github.com/google/uuid"
)

type billingVisibilityReaderFunc func(context.Context) (billinggovernance.Snapshot, error)

func (reader billingVisibilityReaderFunc) Snapshot(ctx context.Context) (billinggovernance.Snapshot, error) {
	return reader(ctx)
}

func TestApprovalDirectoryAndHistoricalVisibilityPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	database, repository, _, _ := newApprovalDeliveryIntegrationDatabase(t)
	repository.approvalPresenterReady = func(policy approvalpolicy.Policy) bool {
		return policy.CommandType == "WALLET_ADJUSTMENT" && policy.PolicyVersion == 1 && policy.SnapshotSchemaVersion == 1
	}
	requester := seedTokenOwnerCandidateActor(t, ctx, database.adminPool, "PLATFORM_BILLING_OPERATOR", "visibility-requester-"+uuid.NewString())
	approver := seedTokenOwnerCandidateActor(t, ctx, database.adminPool, "PLATFORM_BILLING_APPROVER", "visibility-approver-"+uuid.NewString())
	outsider := seedTokenOwnerCandidateActor(t, ctx, database.adminPool, "PERSONAL_USER", "visibility-outsider-"+uuid.NewString())
	policy, err := approvalpolicy.Lookup("WALLET_ADJUSTMENT")
	if err != nil {
		t.Fatal(err)
	}
	requestID := seedAdvancementRequest(t, ctx, repository, requester.ID, policy, "SUBMITTED")

	assertDirectory := func(actor integrationActor, expectedCount int, requestable bool) ApprovalDirectoryProjection {
		t.Helper()
		projection, err := repository.ApprovalDirectory(ctx, approvalIntegrationPrincipal(actor))
		if err != nil {
			t.Fatal(err)
		}
		if projection.SchemaVersion != 1 || projection.PlatformAuthorizationVersion < 1 || len(projection.Items) != expectedCount {
			t.Fatalf("directory projection=%+v", projection)
		}
		if expectedCount == 1 {
			item := projection.Items[0]
			if item.CommandType != policy.CommandType || item.PolicyVersion != policy.PolicyVersion || item.SnapshotSchemaVersion != policy.SnapshotSchemaVersion || !item.Visible || item.Requestable != requestable {
				t.Fatalf("directory item=%+v", item)
			}
		}
		return projection
	}
	requesterDirectory := assertDirectory(requester, 1, true)
	assertDirectory(approver, 1, false)
	assertDirectory(outsider, 0, false)
	originalBillingReader := repository.billingGovernanceActivation
	repository.billingGovernanceActivation = billingVisibilityReaderFunc(func(readCtx context.Context) (billinggovernance.Snapshot, error) {
		snapshot, snapshotErr := originalBillingReader.Snapshot(readCtx)
		if snapshotErr != nil {
			return billinggovernance.Snapshot{}, snapshotErr
		}
		_, updateErr := database.adminPool.Exec(readCtx, `UPDATE authorization_versions SET version=version+1,updated_at=transaction_timestamp() WHERE user_id=$1 AND scope='PLATFORM' AND scope_id='00000000-0000-0000-0000-000000000000'::uuid`, requester.ID)
		return snapshot, updateErr
	})
	if _, err := repository.ApprovalDirectory(ctx, approvalIntegrationPrincipal(requester)); !errors.Is(err, ErrApprovalAuthorizationChanged) {
		t.Fatalf("directory authorization race error=%v", err)
	}
	repository.billingGovernanceActivation = originalBillingReader
	staleRequester := approvalIntegrationPrincipal(requester)
	staleRequester.AuthenticatedAt = time.Now().UTC().Add(-16 * time.Minute)
	staleDirectory, err := repository.ApprovalDirectory(ctx, staleRequester)
	if err != nil || len(staleDirectory.Items) != 1 || staleDirectory.Items[0].Requestable {
		t.Fatalf("stale-auth directory=%+v err=%v", staleDirectory, err)
	}

	if items, err := repository.ListApprovalRequests(ctx, approvalIntegrationPrincipal(requester), ApprovalFilter{}); err != nil || len(items) != 1 || items[0].ID != requestID {
		t.Fatalf("requester history=%+v err=%v", items, err)
	}
	if items, err := repository.ListApprovalRequests(ctx, approvalIntegrationPrincipal(approver), ApprovalFilter{}); err != nil || len(items) != 1 || items[0].ID != requestID {
		t.Fatalf("approver visible set=%+v err=%v", items, err)
	}
	if _, err := repository.ApprovalRequest(ctx, approvalIntegrationPrincipal(outsider), requestID); !errors.Is(err, ErrApprovalRequestNotFound) {
		t.Fatalf("invisible detail error=%v", err)
	}
	if _, err := repository.ApprovalRequest(ctx, approvalIntegrationPrincipal(outsider), uuid.New()); !errors.Is(err, ErrApprovalRequestNotFound) {
		t.Fatalf("missing detail error=%v", err)
	}

	service := &Service{repository: repository}
	invisible := performApprovalIntegrationRequest(t, service.adminApprovalRequest, approvalIntegrationPrincipal(outsider), http.MethodGet, "/admin/v1/approval-requests/"+requestID.String(), requestID.String(), nil)
	missingID := uuid.New()
	missing := performApprovalIntegrationRequest(t, service.adminApprovalRequest, approvalIntegrationPrincipal(outsider), http.MethodGet, "/admin/v1/approval-requests/"+missingID.String(), missingID.String(), nil)
	if invisible.Code != http.StatusNotFound || missing.Code != http.StatusNotFound || invisible.Body.String() != missing.Body.String() || invisible.Header().Get("Cache-Control") != "no-store" || missing.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("non-leaking detail mismatch invisible=%d/%q/%q missing=%d/%q/%q", invisible.Code, invisible.Header().Get("Cache-Control"), invisible.Body.String(), missing.Code, missing.Header().Get("Cache-Control"), missing.Body.String())
	}
	decisionBody := map[string]any{"expected_version": 1, "decision": "REJECTED", "reason": "uniform invisible decision"}
	invisibleDecision := performApprovalIntegrationRequest(t, service.adminDecideApprovalRequest, approvalIntegrationPrincipal(outsider), http.MethodPost, "/admin/v1/approval-requests/"+requestID.String()+"/actions/decide", requestID.String(), decisionBody)
	missingDecision := performApprovalIntegrationRequest(t, service.adminDecideApprovalRequest, approvalIntegrationPrincipal(outsider), http.MethodPost, "/admin/v1/approval-requests/"+missingID.String()+"/actions/decide", missingID.String(), decisionBody)
	if invisibleDecision.Code != http.StatusNotFound || missingDecision.Code != http.StatusNotFound || invisibleDecision.Body.String() != missingDecision.Body.String() || invisibleDecision.Header().Get("Cache-Control") != "no-store" || missingDecision.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("non-leaking decision mismatch invisible=%d/%q/%q missing=%d/%q/%q", invisibleDecision.Code, invisibleDecision.Header().Get("Cache-Control"), invisibleDecision.Body.String(), missingDecision.Code, missingDecision.Header().Get("Cache-Control"), missingDecision.Body.String())
	}

	if _, err := database.adminPool.Exec(ctx, `UPDATE role_assignments SET revoked_at=transaction_timestamp() WHERE user_id=$1 AND role_code='PLATFORM_BILLING_APPROVER' AND revoked_at IS NULL`, approver.ID); err != nil {
		t.Fatal(err)
	}
	if items, err := repository.ListApprovalRequests(ctx, approvalIntegrationPrincipal(approver), ApprovalFilter{}); err != nil || len(items) != 0 {
		t.Fatalf("revoked approver visible set=%+v err=%v", items, err)
	}
	if _, err := repository.ApprovalRequest(ctx, approvalIntegrationPrincipal(approver), requestID); !errors.Is(err, ErrApprovalRequestNotFound) {
		t.Fatalf("revoked detail error=%v", err)
	}
	if _, err := repository.DecideApprovalRequest(ctx, approvalIntegrationPrincipal(approver), requestID, 1, "APPROVED", "revoked approver must not observe writer readiness", nil); !errors.Is(err, ErrApprovalRequestNotFound) {
		t.Fatalf("revoked decision error=%v", err)
	}
	assertDirectory(approver, 0, false)

	if _, err := database.adminPool.Exec(ctx, `UPDATE role_assignments SET revoked_at=transaction_timestamp() WHERE user_id=$1 AND role_code='PLATFORM_BILLING_OPERATOR' AND revoked_at IS NULL`, requester.ID); err != nil {
		t.Fatal(err)
	}
	requesterAfter := assertDirectory(requester, 0, false)
	if requesterAfter.PlatformAuthorizationVersion <= requesterDirectory.PlatformAuthorizationVersion {
		t.Fatalf("authorization version did not advance: before=%d after=%d", requesterDirectory.PlatformAuthorizationVersion, requesterAfter.PlatformAuthorizationVersion)
	}
	if item, err := repository.ApprovalRequest(ctx, approvalIntegrationPrincipal(requester), requestID); err != nil || item.ID != requestID || len(item.AllowedActions) != 0 {
		t.Fatalf("revoked requester history=%+v err=%v", item, err)
	}
}
