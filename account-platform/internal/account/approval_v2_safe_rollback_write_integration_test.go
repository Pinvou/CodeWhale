package account

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

// The frozen mode matrix (redesign sections 5.4 and 8.7) closes
// create/update/submit from the SAFE CAS onward with a stable
// 503 APPROVAL_SAFE_ROLLBACK and zero side effects, while WITHDRAW stays
// available for a SUBMITTED request. v1 callers must be closed too: the Portal
// routes a v1-locked session to the v1 write paths, so "the browser cannot get
// there" is not an enforcement argument.
func TestApprovalDraftWritesClosedDuringSafeRollbackIntegration(t *testing.T) {
	db := newApprovalAuthorizationIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	actor := seedTokenOwnerCandidateActor(t, ctx, db.adminPool, "PLATFORM_SUPER_ADMIN", "safe-draft-"+uuid.NewString())
	policy, err := approvalpolicy.Lookup("WALLET_ADJUSTMENT")
	if err != nil {
		t.Fatal(err)
	}
	seed := &PostgresRepository{pool: db.adminPool, now: func() time.Time { return time.Now().UTC() }}
	draftID := seedAdvancementRequest(t, ctx, seed, actor.ID, policy, "DRAFT")
	submittedID := seedAdvancementRequest(t, ctx, seed, actor.ID, policy, "SUBMITTED")
	repo := (&PostgresRepository{pool: db.runtimePool, now: func() time.Time { return time.Now().UTC() }}).
		WithBillingGovernanceActivation(&billingAdvancementReader{snapshot: billingAdvancementSnapshot("ACTIVE")})
	service := &Service{repository: repo}
	invoke := func(handler http.HandlerFunc, method, path, id, version string, body any) *httptest.ResponseRecorder {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(method, path, bytes.NewReader(raw))
		if id != "" {
			request.SetPathValue("approval_request_id", id)
		}
		request.Header.Set("Authorization", "Bearer integration")
		request.Header.Set("Content-Type", "application/json")
		if version != "" {
			request.Header.Set("X-Pinvou-Approval-Action-Control-Version", version)
		}
		response := httptest.NewRecorder()
		identity.Middleware(func(context.Context, string) (identity.Principal, error) {
			return approvalIntegrationPrincipal(actor), nil
		}, handler).ServeHTTP(response, request)
		return response
	}
	countRows := func() int {
		t.Helper()
		var count int
		if err := db.adminPool.QueryRow(ctx, `SELECT count(*) FROM approval_requests WHERE requester_user_id=$1`, actor.ID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	newDraftBody := func() map[string]any {
		draft := approvalTestDraft()
		draft.IdempotencyKey = uuid.NewString()
		return approvalDraftBodyFromModel(draft)
	}
	before, err := repo.ApprovalActionControl(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Positive control: before the SAFE CAS the very same create succeeds, so
	// any later refusal is attributable to the mode and not to the fixture.
	open := invoke(service.adminCreateApprovalDraftV2, http.MethodPost, "/admin/v2/approval-requests", "",
		strconv.FormatInt(before.Version, 10), newDraftBody())
	if open.Code != http.StatusCreated {
		t.Fatalf("pre-SAFE v2 create: %d %s", open.Code, open.Body.String())
	}
	rowsBeforeSafe := countRows()
	setApprovalConsumerControl(t, ctx, db, "SAFE_ROLLBACK", "RUNNING", 1, true, before.ApprovalEpoch+1)
	safe, err := repo.ApprovalActionControl(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if safe.Mode != AdminApprovalModeSafeRollback || safe.Version <= before.Version {
		t.Fatalf("fixture did not enter SAFE_ROLLBACK: %s/%d", safe.Mode, safe.Version)
	}
	version := strconv.FormatInt(safe.Version, 10)
	closed := []struct {
		name    string
		handler http.HandlerFunc
		method  string
		path    string
		id      string
		header  string
		body    any
	}{
		{"v2 create", service.adminCreateApprovalDraftV2, http.MethodPost, "/admin/v2/approval-requests", "", version, newDraftBody()},
		{"v1 create", service.adminCreateApprovalDraft, http.MethodPost, "/admin/v1/approval-requests", "", "", newDraftBody()},
		{"v2 update", service.adminUpdateApprovalDraftV2, http.MethodPut, "/admin/v2/approval-requests/" + draftID.String(), draftID.String(), version,
			func() map[string]any { body := newDraftBody(); body["expected_version"] = 1; return body }()},
		{"v1 update", service.adminUpdateApprovalDraft, http.MethodPut, "/admin/v1/approval-requests/" + draftID.String(), draftID.String(), "",
			func() map[string]any { body := newDraftBody(); body["expected_version"] = 1; return body }()},
		{"v2 submit", service.adminSubmitApprovalRequestV2, http.MethodPost, "/admin/v2/approval-requests/" + draftID.String() + "/actions/submit", draftID.String(), version,
			map[string]any{"expected_version": 1}},
		{"v1 submit", service.adminSubmitApprovalRequest, http.MethodPost, "/admin/v1/approval-requests/" + draftID.String() + "/actions/submit", draftID.String(), "",
			map[string]any{"expected_version": 1}},
	}
	for _, test := range closed {
		response := invoke(test.handler, test.method, test.path, test.id, test.header, test.body)
		if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "APPROVAL_SAFE_ROLLBACK") ||
			response.Header().Get("Retry-After") != "5" || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s during SAFE_ROLLBACK: %d %s", test.name, response.Code, response.Body.String())
		}
	}
	if rows := countRows(); rows != rowsBeforeSafe {
		t.Fatalf("SAFE_ROLLBACK admitted a new approval request: %d -> %d", rowsBeforeSafe, rows)
	}
	draft, err := approvalRequestByID(ctx, db.adminPool, draftID)
	if err != nil || draft.Status != "DRAFT" || draft.Version != 1 {
		t.Fatalf("SAFE_ROLLBACK mutated the editable draft: %s/%d %v", draft.Status, draft.Version, err)
	}
	// The frozen matrix keeps the safe close path open for a SUBMITTED request.
	withdraw := invoke(service.adminWithdrawApprovalRequestV2, http.MethodPost, "/admin/v2/approval-requests/"+submittedID.String()+"/actions/withdraw",
		submittedID.String(), version, map[string]any{"expected_version": 1})
	if withdraw.Code != http.StatusOK || !strings.Contains(withdraw.Body.String(), `"status":"WITHDRAWN"`) {
		t.Fatalf("SAFE_ROLLBACK closed the withdraw path: %d %s", withdraw.Code, withdraw.Body.String())
	}
}
