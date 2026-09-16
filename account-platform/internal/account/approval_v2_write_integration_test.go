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

func TestApprovalV2WithdrawUsesSameRequestAndControlCASIntegration(t *testing.T) {
	db := newApprovalAuthorizationIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	actor := seedTokenOwnerCandidateActor(t, ctx, db.adminPool, "PLATFORM_SUPER_ADMIN", "v2-withdraw-"+uuid.NewString())
	policy, err := approvalpolicy.Lookup("WALLET_ADJUSTMENT")
	if err != nil {
		t.Fatal(err)
	}
	seed := &PostgresRepository{pool: db.adminPool, now: func() time.Time { return time.Now().UTC() }}
	id := seedAdvancementRequest(t, ctx, seed, actor.ID, policy, "SUBMITTED")
	repo := &PostgresRepository{pool: db.runtimePool, now: func() time.Time { return time.Now().UTC() }}
	control, err := repo.ApprovalActionControl(ctx)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{repository: repo}
	invoke := func(version string, body any) *httptest.ResponseRecorder {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/admin/v2/approval-requests/"+id.String()+"/actions/withdraw", bytes.NewReader(raw))
		request.SetPathValue("approval_request_id", id.String())
		request.Header.Set("Authorization", "Bearer integration")
		request.Header.Set("Content-Type", "application/json")
		if version != "" {
			request.Header.Set("X-Pinvou-Approval-Action-Control-Version", version)
		}
		response := httptest.NewRecorder()
		identity.Middleware(func(context.Context, string) (identity.Principal, error) {
			return approvalIntegrationPrincipal(actor), nil
		}, http.HandlerFunc(service.adminWithdrawApprovalRequestV2)).ServeHTTP(response, request)
		return response
	}
	for _, version := range []string{"", "01", strconv.FormatInt(control.Version+1, 10)} {
		response := invoke(version, map[string]any{"expected_version": 1})
		if version == strconv.FormatInt(control.Version+1, 10) {
			if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "APPROVAL_ACTION_CONTROL_CHANGED") {
				t.Fatalf("stale control version: %d %s", response.Code, response.Body.String())
			}
		} else if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid control version: %d %s", response.Code, response.Body.String())
		}
	}
	response := invoke(strconv.FormatInt(control.Version, 10), map[string]any{"expected_version": 1, "proof": map[string]any{}})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("browser proof accepted: %d %s", response.Code, response.Body.String())
	}
	response = invoke(strconv.FormatInt(control.Version, 10), map[string]any{"expected_version": 1})
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"WITHDRAWN"`) ||
		strings.Contains(response.Body.String(), "normalized_command") || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("v2 close returned raw or failed: %d %s", response.Code, response.Body.String())
	}
	item, err := approvalRequestByID(ctx, db.adminPool, id)
	if err != nil || item.Status != "WITHDRAWN" || item.Version != 2 {
		t.Fatalf("v2 write did not use original request: %s/%d %v", item.Status, item.Version, err)
	}
}

func TestApprovalV2DraftLifecycleIntegration(t *testing.T) {
	db := newApprovalAuthorizationIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	actor := seedTokenOwnerCandidateActor(t, ctx, db.adminPool, "PLATFORM_SUPER_ADMIN", "v2-draft-"+uuid.NewString())
	repo := (&PostgresRepository{pool: db.runtimePool, now: func() time.Time { return time.Now().UTC() }}).
		WithBillingGovernanceActivation(&billingAdvancementReader{snapshot: billingAdvancementSnapshot("ACTIVE")})
	control, err := repo.ApprovalActionControl(ctx)
	if err != nil {
		t.Fatal(err)
	}
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
		request.Header.Set("X-Pinvou-Approval-Action-Control-Version", version)
		response := httptest.NewRecorder()
		identity.Middleware(func(context.Context, string) (identity.Principal, error) {
			return approvalIntegrationPrincipal(actor), nil
		}, handler).ServeHTTP(response, request)
		return response
	}
	version := strconv.FormatInt(control.Version, 10)
	draft := approvalTestDraft()
	draft.IdempotencyKey = uuid.NewString()
	body := approvalDraftBodyFromModel(draft)
	stale := invoke(service.adminCreateApprovalDraftV2, http.MethodPost, "/admin/v2/approval-requests", "", strconv.FormatInt(control.Version+1, 10), body)
	if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), "APPROVAL_ACTION_CONTROL_CHANGED") {
		t.Fatalf("stale create: %d %s", stale.Code, stale.Body.String())
	}
	created := invoke(service.adminCreateApprovalDraftV2, http.MethodPost, "/admin/v2/approval-requests", "", version, body)
	if created.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	var detail struct {
		ID      uuid.UUID `json:"approval_request_id"`
		Status  string    `json:"status"`
		Version int64     `json:"version"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &detail); err != nil || detail.ID == uuid.Nil || detail.Status != "DRAFT" || detail.Version != 1 {
		t.Fatalf("created detail: %s %v", created.Body.String(), err)
	}
	draft.Reason = "updated independent financial repair"
	updateBody := approvalDraftBodyFromModel(draft)
	updateBody["expected_version"] = detail.Version
	updated := invoke(service.adminUpdateApprovalDraftV2, http.MethodPut, "/admin/v2/approval-requests/"+detail.ID.String(), detail.ID.String(), version, updateBody)
	if updated.Code != http.StatusOK || !strings.Contains(updated.Body.String(), `"version":2`) {
		t.Fatalf("update: %d %s", updated.Code, updated.Body.String())
	}
	submitted := invoke(service.adminSubmitApprovalRequestV2, http.MethodPost, "/admin/v2/approval-requests/"+detail.ID.String()+"/actions/submit", detail.ID.String(), version, map[string]any{"expected_version": 2})
	if submitted.Code != http.StatusOK || !strings.Contains(submitted.Body.String(), `"status":"SUBMITTED"`) {
		t.Fatalf("submit: %d %s", submitted.Code, submitted.Body.String())
	}
	withdrawn := invoke(service.adminWithdrawApprovalRequestV2, http.MethodPost, "/admin/v2/approval-requests/"+detail.ID.String()+"/actions/withdraw", detail.ID.String(), version, map[string]any{"expected_version": 3})
	if withdrawn.Code != http.StatusOK || !strings.Contains(withdrawn.Body.String(), `"status":"WITHDRAWN"`) {
		t.Fatalf("withdraw: %d %s", withdrawn.Code, withdrawn.Body.String())
	}
	item, err := approvalRequestByID(ctx, db.adminPool, detail.ID)
	if err != nil || item.Version != 4 || item.Status != "WITHDRAWN" {
		t.Fatalf("persisted lifecycle: %s/%d %v", item.Status, item.Version, err)
	}
}
