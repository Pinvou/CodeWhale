package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
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
	var seeded []uuid.UUID
	for i := 0; i < 3; i++ {
		seeded = append(seeded, seedAdvancementRequest(t, ctx, repository, actor.ID, policy, "SUBMITTED"))
	}
	principal := approvalIntegrationPrincipal(actor)
	item, err := repository.ApprovalRequest(ctx, principal, seeded[0])
	if err != nil {
		t.Fatal(err)
	}
	if projection, err := repository.approvalV2Execution(ctx, item); err != nil || projection != nil {
		t.Fatalf("execution absent projection=%+v err=%v", projection, err)
	}
	// A malformed historical execution for another command must not be
	// attached to this request's closed detail.
	if _, err := database.adminPool.Exec(ctx, `INSERT INTO approval_executions
 (id,approval_request_id,command_type,command_fingerprint,execution_idempotency_key,executor_identity,job_run_id,status,authorization_snapshot,started_at)
	 VALUES($1,$2,'PAYMENT_PRODUCT_RESUME',decode(repeat('ab',32),'hex'),$3,'billing-command-executor','foreign-command','EXECUTING','{}',now())`, uuid.New(), seeded[0], uuid.NewString()); err == nil {
		t.Fatal("database accepted an unrelated execution")
	}
	if projection, err := repository.approvalV2Execution(ctx, item); err != nil || projection != nil {
		t.Fatalf("foreign execution leaked projection=%+v err=%v", projection, err)
	}
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
	fixtureBytes, err := os.ReadFile("../platform/approvalencoding/testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		ProofVectors []struct {
			CommandType string `json:"command_type"`
			Legacy      struct {
				TargetResourceType string `json:"target_resource_type"`
				TargetResourceID   string `json:"target_resource_id"`
				NormalizedJSON     string `json:"normalized_json"`
				CriticalJSON       string `json:"critical_json"`
				FinancialJSON      string `json:"financial_json"`
			} `json:"legacy"`
		} `json:"proof_vectors"`
	}
	if err := json.Unmarshal(fixtureBytes, &fixture); err != nil {
		t.Fatal(err)
	}
	wallet := fixture.ProofVectors[0]
	if wallet.CommandType != "WALLET_ADJUSTMENT" {
		t.Fatal("wallet golden was reordered")
	}
	validID := seedAdvancementRequest(t, ctx, repository, actor.ID, policy, "DRAFT")
	if _, err := database.adminPool.Exec(ctx, `UPDATE approval_requests SET normalized_command=$2,target_resource_type=$3,target_resource_id=$4 WHERE id=$1`, validID,
		wallet.Legacy.NormalizedJSON, wallet.Legacy.TargetResourceType, wallet.Legacy.TargetResourceID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.adminPool.Exec(ctx, `UPDATE approval_request_snapshots SET critical_resource_versions=$2,financial_snapshot=$3 WHERE approval_request_id=$1`, validID,
		wallet.Legacy.CriticalJSON, wallet.Legacy.FinancialJSON); err != nil {
		t.Fatal(err)
	}
	response := performApprovalIntegrationRequest(t, service.adminApprovalRequestV2, principal, http.MethodGet,
		"/admin/v2/approval-requests/"+validID.String(), validID.String(), nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"amount_microcredits":"9223372036854775807"`) || !strings.Contains(response.Body.String(), `"action_control_version":`) || !strings.Contains(response.Body.String(), `"execution":null`) {
		t.Fatalf("closed wallet detail: %d %s", response.Code, response.Body.String())
	}
	for _, forbidden := range []string{"normalized_command", "critical_resource_versions", "financial_snapshot", "requested_by", "command_fingerprint"} {
		if strings.Contains(response.Body.String(), forbidden) {
			t.Fatalf("closed detail leaked %s", forbidden)
		}
	}
}

func TestApprovalV2ListTimeoutClassification(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := approvalV2ListError(context.DeadlineExceeded, ctx, "cursor"); !errors.Is(err, ErrApprovalPaginationQueryTimeout) {
		t.Fatalf("continuation timeout=%v", err)
	}
	if err := approvalV2ListError(context.DeadlineExceeded, ctx, ""); !errors.Is(err, ErrApprovalPaginationSnapshotUnavailable) {
		t.Fatalf("build timeout=%v", err)
	}
}
