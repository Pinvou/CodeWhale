package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/google/uuid"
)

func TestApprovalV2ListPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	database, repository, _ := newApprovalPaginationIntegrationDatabase(t)
	if err := repository.ConfigureApprovalPagination(ctx, ApprovalPaginationConfig{Enabled: true, CurrentKeyID: "v2-list", CurrentKey: []byte(strings.Repeat("k", 32))}); err != nil {
		t.Fatal(err)
	}
	if err := repository.SetApprovalPaginationGate(ctx, true); err != nil {
		t.Fatal(err)
	}
	actor := seedTokenOwnerCandidateActor(t, ctx, database.adminPool, "PLATFORM_BILLING_OPERATOR", "v2-list-"+uuid.NewString())
	policy, err := approvalpolicy.Lookup("WALLET_ADJUSTMENT")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		seedAdvancementRequest(t, ctx, repository, actor.ID, policy, "SUBMITTED")
	}
	principal := approvalIntegrationPrincipal(actor)
	page, err := repository.ListApprovalRequestsV2(ctx, principal, ApprovalFilter{}, "", 2, "127.0.0.1")
	if err != nil || len(page.Items) != 2 || page.NextCursor == nil || !page.HasMore {
		t.Fatalf("first page=%+v err=%v", page, err)
	}
	encoded, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"normalized_command", "critical_resource_versions", "financial_snapshot", "allowed_actions", "historical approval containment"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("list leaked %s: %s", forbidden, encoded)
		}
	}
	continuation, err := repository.ListApprovalRequestsV2(ctx, principal, ApprovalFilter{}, *page.NextCursor, 2, "127.0.0.1")
	if err != nil || len(continuation.Items) != 1 || continuation.HasMore {
		t.Fatalf("continuation=%+v err=%v", continuation, err)
	}
	if _, err = database.adminPool.Exec(ctx, `UPDATE authorization_versions SET version=version+1,updated_at=transaction_timestamp() WHERE user_id=$1 AND scope='PLATFORM' AND scope_id='00000000-0000-0000-0000-000000000000'::uuid`, actor.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ListApprovalRequestsV2(ctx, principal, ApprovalFilter{}, *page.NextCursor, 2, "127.0.0.1"); !errors.Is(err, ErrApprovalPaginationCursorExpired) {
		t.Fatalf("authorization change=%v", err)
	}

	service := NewService(repository, nil)
	bad := performApprovalIntegrationRequest(t, service.adminApprovalRequestsV2, principal, http.MethodGet, "/admin/v2/approval-requests?limit=201", "", nil)
	if bad.Code != http.StatusBadRequest || !strings.Contains(bad.Body.String(), "INVALID_LIMIT") {
		t.Fatalf("bad limit: %d %s", bad.Code, bad.Body.String())
	}
	bad = performApprovalIntegrationRequest(t, service.adminApprovalRequestsV2, principal, http.MethodGet, "/admin/v2/approval-requests?status=SUBMITTED&status=APPROVED", "", nil)
	if bad.Code != http.StatusBadRequest || !strings.Contains(bad.Body.String(), "INVALID_FILTER") {
		t.Fatalf("duplicate filter: %d %s", bad.Code, bad.Body.String())
	}
}
