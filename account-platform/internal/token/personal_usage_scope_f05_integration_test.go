package token

import (
	"encoding/json"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestF05PersonalHistoryAndCancellationPersistedOwnerScope(t *testing.T) {
	r, ctx := retainedManagementTokenRepository(t)
	funds, _, financePool := retainedManagementBillingRepository(t)
	user := uuid.New()
	wallet, _, err := funds.EnsureWallet(ctx, "PERSONAL", user)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = funds.Credit(ctx, wallet.ID, "scope-f05-"+uuid.NewString(), 1000, false); err != nil {
		t.Fatal(err)
	}
	usage := retainedCancellationUsage(t, r, ctx, user, wallet.ID, nil, "STREAMING")
	originalKey := "f05-original-invocation-key"
	if _, err = r.pool.Exec(ctx, `UPDATE usage_requests SET idempotency_namespace='model-inference',idempotency_key_digest=$2 WHERE id=$1`, usage, digestString([]byte(originalKey))); err != nil {
		t.Fatal(err)
	}
	hold, err := funds.Reserve(ctx, usage, wallet.ID, 100, nil, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.pool.Exec(ctx, `UPDATE usage_requests SET hold_id=$2,cancel_status='NOT_REQUESTED' WHERE id=$1`, usage, hold.ID); err != nil {
		t.Fatal(err)
	}
	org := uuid.New()
	organizationUsage := retainedCancellationUsage(t, r, ctx, user, wallet.ID, nil, "STREAMING")
	foreignUsage := retainedCancellationUsage(t, r, ctx, uuid.New(), wallet.ID, nil, "STREAMING")
	organizationKey, foreignKey := "f05-organization-invocation-key", "f05-foreign-invocation-key"
	for id, key := range map[uuid.UUID]string{organizationUsage: organizationKey, foreignUsage: foreignKey} {
		if _, err = r.pool.Exec(ctx, `UPDATE usage_requests SET idempotency_namespace='model-inference',idempotency_key_digest=$2 WHERE id=$1`, id, digestString([]byte(key))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = r.pool.Exec(ctx, `UPDATE usage_requests SET organization_id=$2,cancel_status='NOT_REQUESTED' WHERE id=$1`, organizationUsage, org); err != nil {
		t.Fatal(err)
	}
	s, decisions, closeAuth := newModelUsageAuthorizationService(t, AuthorizationContext{UserID: user, SessionID: "new-login-session", ContextType: "PERSONAL"})
	defer closeAuth()
	s.repository = r
	request := func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer controlled-token")
		req.Header.Set("Idempotency-Key", "f05-cancel-controlled-key")
		out := httptest.NewRecorder()
		s.Handler(successfulBillingVerify).ServeHTTP(out, req)
		return out
	}
	list := request("GET", "/v1/me/usage-requests")
	if list.Code != 200 || !strings.Contains(list.Body.String(), usage.String()) || strings.Contains(list.Body.String(), organizationUsage.String()) || strings.Contains(list.Body.String(), foreignUsage.String()) {
		t.Fatal("owner history boundary", list.Code, list.Body.String())
	}
	detail := request("GET", "/v1/usage-requests/"+usage.String())
	if detail.Code != 200 || detail.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("new login cannot inspect personal history", detail.Code)
	}
	lookup := func(key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/v1/usage-requests/by-idempotency-key", nil)
		req.Header.Set("Authorization", "Bearer controlled-token")
		req.Header.Set("Idempotency-Key", key)
		out := httptest.NewRecorder()
		s.Handler(successfulBillingVerify).ServeHTTP(out, req)
		return out
	}
	if got := lookup(originalKey); got.Code != 200 || !strings.Contains(got.Body.String(), usage.String()) || got.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("original identity not recovered", got.Code, got.Body.String())
	}
	if got := lookup("f05-nonexistent-original-key"); got.Code != 404 {
		t.Fatal("unknown identity must remain unknown", got.Code)
	}
	for _, key := range []string{organizationKey, foreignKey} {
		if got := lookup(key); got.Code != 404 {
			t.Fatal("lookup escaped personal owner scope", got.Code)
		}
	}
	if got := lookup("short"); got.Code != 400 {
		t.Fatal("invalid original key accepted", got.Code)
	}
	var writes int
	if err = r.pool.QueryRow(ctx, `SELECT count(*) FROM usage_cancel_commands WHERE usage_request_id=$1`, usage).Scan(&writes); err != nil || writes != 0 {
		t.Fatal("read-only recovery wrote cancellation", writes, err)
	}
	for _, id := range []uuid.UUID{organizationUsage, foreignUsage} {
		if got := request("GET", "/v1/usage-requests/"+id.String()); got.Code != 404 {
			t.Fatal("foreign detail accessible", got.Code)
		}
		if got := request("POST", "/v1/usage-requests/"+id.String()+"/cancel"); got.Code != 404 {
			t.Fatal("foreign cancellation accepted", got.Code)
		}
		var count int
		if err = r.pool.QueryRow(ctx, `SELECT count(*) FROM usage_cancel_commands WHERE usage_request_id=$1`, id).Scan(&count); err != nil || count != 0 {
			t.Fatal("rejected resource wrote cancellation", count, err)
		}
	}
	// Selecting an organization must not remove the existing ability to stop a
	// personal invocation. Scope is the persisted target, not the selection.
	s.contexts = fixedBillingContextResolver{resolved: AuthorizationContext{UserID: user, SessionID: "new-login-session", ContextType: "ORGANIZATION", OrganizationID: &org}}
	cancelled := request("POST", "/v1/usage-requests/"+usage.String()+"/cancel")
	if cancelled.Code != 200 || !strings.Contains(cancelled.Body.String(), `"cancel_status":"UNSUPPORTED"`) {
		t.Fatal("in-flight cancellation changed policy", cancelled.Code, cancelled.Body.String())
	}
	if replay := request("POST", "/v1/usage-requests/"+usage.String()+"/cancel"); replay.Code != 200 {
		t.Fatal("same command replay failed", replay.Code)
	}
	current, err := r.GetUsage(ctx, usage)
	if err != nil || current.ExecutionStatus != "STREAMING" || current.DeliveryStatus != "USER_STOPPED" {
		t.Fatal("cancelled invocation lost dispatch state", err)
	}
	var reserved int64
	var status string
	if err = financePool.QueryRow(ctx, `SELECT w.reserved_microcredits,h.status FROM wallets w JOIN usage_holds h ON h.wallet_id=w.id WHERE h.id=$1`, hold.ID).Scan(&reserved, &status); err != nil || reserved != 100 || status != "ACTIVE" {
		t.Fatal("dispatch cancellation released funds", reserved, status, err)
	}
	decisions.allowed = false
	if denied := request("POST", "/v1/usage-requests/"+usage.String()+"/cancel"); denied.Code != 403 {
		t.Fatal("revoked cancel permission accepted", denied.Code)
	}
}

func TestF05AdminUsageGateOffUsesSafeCollectionProjection(t *testing.T) {
	r, ctx := retainedManagementTokenRepository(t)
	user := uuid.New()
	usage := retainedCancellationUsage(t, r, ctx, user, uuid.New(), nil, "STREAMING")
	if err := r.SetAdminPaginationGate(ctx, false); err != nil {
		t.Fatal(err)
	}
	s, decisions, closeAuth := newModelUsageAuthorizationService(t, AuthorizationContext{UserID: user, ContextType: "PERSONAL"})
	defer closeAuth()
	s.WithManagement(r, nil, ProviderURLPolicy{}, "", successfulBillingVerify)
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/usage-requests?limit=20", nil)
	req.Header.Set("Authorization", "Bearer controlled-token")
	out := httptest.NewRecorder()
	s.Handler(successfulBillingVerify).ServeHTTP(out, req)
	if out.Code != 200 {
		t.Fatal("gate-off registered endpoint", out.Code, out.Body.String())
	}
	var response struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(out.Body.Bytes(), &response); err != nil || len(response.Items) != 1 {
		t.Fatal("collection response", err)
	}
	item := response.Items[0]
	for _, key := range []string{"user_id", "session_id", "credential_id", "wallet_id", "provider_id", "request_fingerprint", "upstream_request_id", "cancellation_evidence", "convergence_evidence"} {
		if _, present := item[key]; present {
			t.Fatal("collection leaked field", key)
		}
	}
	var id string
	_ = json.Unmarshal(item["usage_request_id"], &id)
	if id != usage.String() || string(item["execution_status"]) != `"STREAMING"` || string(item["maximum_microcredits"]) != "100" {
		t.Fatal("necessary collection facts lost")
	}
	decisions.allowed = false
	denied := httptest.NewRecorder()
	s.Handler(successfulBillingVerify).ServeHTTP(denied, req)
	if denied.Code != 403 {
		t.Fatal("unauthorized collection accessible", denied.Code)
	}
}
