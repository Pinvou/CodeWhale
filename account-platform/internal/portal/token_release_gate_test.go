package portal

import (
	"context"
	"github.com/google/uuid"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSessionEndpointExposesOnlyNonSensitiveAdminModelTokenIAGate(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	service.config.AdminModelTokenIAEnabled = true
	session := Session{ID: "feature-session", AccessToken: "access-secret", RefreshToken: "refresh-secret", AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(2 * time.Hour), CSRFToken: strings.Repeat("f", 32)}
	_ = store.SaveSession(context.Background(), session)
	request := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)
	if body := response.Body.String(); response.Code != http.StatusOK || !strings.Contains(body, `"admin_model_token_ia":true`) || strings.Contains(body, "access-secret") || strings.Contains(body, "refresh-secret") {
		t.Fatalf("unexpected feature session response: %d %s", response.Code, body)
	}
}

func TestSessionEndpointExposesOnlyNonSensitiveTokenResourceFormsGate(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	service.config.AdminTokenResourceFormsEnabled = true
	session := Session{ID: "token-resource-feature-session", AccessToken: "access-secret", RefreshToken: "refresh-secret", AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(2 * time.Hour), CSRFToken: strings.Repeat("t", 32)}
	_ = store.SaveSession(context.Background(), session)
	request := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)
	if body := response.Body.String(); response.Code != http.StatusOK || !strings.Contains(body, `"admin_token_resource_forms":true`) || strings.Contains(body, "access-secret") || strings.Contains(body, "refresh-secret") {
		t.Fatalf("unexpected token resource feature session response: %d %s", response.Code, body)
	}
}

func TestSessionEndpointExposesOnlyNonSensitiveModelOperationsDetailsGate(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	service.config.AdminModelOperationsDetailsEnabled = true
	session := Session{ID: "model-operations-feature-session", AccessToken: "access-secret", RefreshToken: "refresh-secret", AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(2 * time.Hour), CSRFToken: strings.Repeat("o", 32)}
	_ = store.SaveSession(context.Background(), session)
	request := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)
	if body := response.Body.String(); response.Code != http.StatusOK || !strings.Contains(body, `"admin_model_operations_details":true`) || strings.Contains(body, "access-secret") || strings.Contains(body, "refresh-secret") {
		t.Fatalf("unexpected model operations feature session response: %d %s", response.Code, body)
	}
}

func TestSessionEndpointExposesOnlyNonSensitiveModelOperationsOverviewGate(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	service.config.AdminModelOperationsOverviewEnabled = true
	session := Session{ID: "model-operations-overview-session", AccessToken: "access-secret", RefreshToken: "refresh-secret", AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(2 * time.Hour), CSRFToken: strings.Repeat("v", 32)}
	_ = store.SaveSession(context.Background(), session)
	request := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)
	if body := response.Body.String(); response.Code != http.StatusOK || !strings.Contains(body, `"admin_model_operations_overview":true`) || strings.Contains(body, "access-secret") || strings.Contains(body, "refresh-secret") {
		t.Fatalf("unexpected model operations overview feature session response: %d %s", response.Code, body)
	}
}

func TestSessionEndpointExposesOnlyNonSensitiveModelTokenPageOverviewsGate(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	service.config.AdminModelTokenPageOverviewsEnabled = true
	service.config.TokenAdminProxyHMACKey = []byte(strings.Repeat("p", 32))
	session := Session{ID: "model-token-page-overviews-session", AccessToken: "access-secret", RefreshToken: "refresh-secret", AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(2 * time.Hour), CSRFToken: strings.Repeat("w", 32)}
	_ = store.SaveSession(context.Background(), session)
	request := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)
	if body := response.Body.String(); response.Code != http.StatusOK || !strings.Contains(body, `"admin_model_token_page_overviews":true`) || strings.Contains(body, "access-secret") || strings.Contains(body, "refresh-secret") {
		t.Fatalf("unexpected model and Token page overview feature session response: %d %s", response.Code, body)
	}
}

func TestSessionEndpointExposesOnlyNonSensitiveModelDetailsGate(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	service.config.AdminModelDetailsEnabled = true
	session := Session{ID: "model-details-feature-session", AccessToken: "access-secret", RefreshToken: "refresh-secret", AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(2 * time.Hour), CSRFToken: strings.Repeat("d", 32)}
	_ = store.SaveSession(context.Background(), session)
	request := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)
	if body := response.Body.String(); response.Code != http.StatusOK || !strings.Contains(body, `"admin_model_details":true`) || strings.Contains(body, "access-secret") || strings.Contains(body, "refresh-secret") {
		t.Fatalf("unexpected model details feature session response: %d %s", response.Code, body)
	}
}

func TestSessionEndpointExposesOnlyNonSensitiveModelPricingEditGate(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	service.config.AdminModelPricingEditEnabled = true
	session := Session{ID: "model-pricing-edit-session", AccessToken: "access-secret", RefreshToken: "refresh-secret", AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(2 * time.Hour), CSRFToken: strings.Repeat("p", 32)}
	_ = store.SaveSession(context.Background(), session)
	request := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)
	if body := response.Body.String(); response.Code != http.StatusOK || !strings.Contains(body, `"admin_model_pricing_edit":true`) || strings.Contains(body, "access-secret") || strings.Contains(body, "refresh-secret") {
		t.Fatalf("unexpected model pricing edit feature session response: %d %s", response.Code, body)
	}
}

func TestSessionEndpointExposesOnlyNonSensitiveModelTokenActionsGate(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	service.config.AdminModelTokenActionsEnabled = true
	session := Session{ID: "model-token-actions-session", AccessToken: "access-secret", RefreshToken: "refresh-secret", AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(2 * time.Hour), CSRFToken: strings.Repeat("a", 32)}
	_ = store.SaveSession(context.Background(), session)
	request := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)
	if body := response.Body.String(); response.Code != http.StatusOK || !strings.Contains(body, `"admin_model_token_actions":true`) || strings.Contains(body, "access-secret") || strings.Contains(body, "refresh-secret") {
		t.Fatalf("unexpected model Token actions feature session response: %d %s", response.Code, body)
	}
}

// Use the current version-neutral BFF, real capability parser/wire lock and
// authoritative detail to exercise the recovered local gate. No Account DB is simulated.
func TestModelTokenGateUsesCurrentApprovalBFFClassification(t *testing.T) {
	for _, tc := range []struct {
		name, command, alter string
		want                 int
	}{
		{"token-blocked", "TOKEN_MODEL_LIFECYCLE_CHANGE", "", 404},
		{"finance-remains-allowed", "WALLET_ADJUSTMENT", "", 204},
		{"expired-classification", "WALLET_ADJUSTMENT", "expired", 503},
		{"actor-replacement", "WALLET_ADJUSTMENT", "actor", 503},
		{"cross-session", "WALLET_ADJUSTMENT", "session", 503},
		{"missing-classification", "WALLET_ADJUSTMENT", "missing", 503}} {
		t.Run(tc.name, func(t *testing.T) {
			store := &financeWireStore{memoryStore: newMemoryStore(), wire: map[string]int{}}
			s := testService(t, store)
			s.config.AdminModelTokenActionsEnabled = false
			session := validSession(uuid.NewString())
			session.ApprovalV2Eligible = true
			if e := store.SaveSession(context.Background(), session); e != nil {
				t.Fatal(e)
			}
			id := uuid.NewString()
			var writes atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Header.Get("Authorization") != "Bearer "+session.AccessToken {
					t.Error("server session authority changed")
				}
				if r.URL.Path == "/admin/v2/approval-capability" {
					w.Header().Set("Cache-Control", "no-store")
					io.WriteString(w, `{"schema_version":1,"mode":"PROOF_REQUIRED","approval_wire_version":2,"proof_required":true,"epoch":1,"authorization_relay_state":"RUNNING","authorization_relay_epoch":1,"version":1,"request_id":"fixture"}`)
					return
				}
				if r.Method == "GET" && r.URL.Path == "/admin/v2/approval-requests/"+id {
					io.WriteString(w, `{"approval_request_id":"`+id+`","command_type":"`+tc.command+`","action_control_version":1,"detail_availability":"AVAILABLE"}`)
					return
				}
				writes.Add(1)
				w.WriteHeader(204)
			}))
			defer upstream.Close()
			s.config.AccountURL = parseTestURL(t, upstream.URL)
			detail := financeBrowserRequest(s, session, "GET", "/api/admin/approvals/requests/"+id, "")
			rr := httptest.NewRecorder()
			s.Handler(nil).ServeHTTP(rr, detail)
			if rr.Code != 200 {
				t.Fatalf("detail status %d: %s", rr.Code, rr.Body.String())
			}
			proof, e := store.GetApprovalClassification(context.Background(), session.ID, id)
			if e != nil || proof.CommandType != tc.command {
				t.Fatal("authoritative detail not classified")
			}
			switch tc.alter {
			case "expired":
				proof.ExpiresAt = s.now().Add(-time.Second)
				store.SaveApprovalClassification(context.Background(), proof)
			case "actor":
				proof.ActorTokenFingerprint = approvalActorTokenFingerprint("different actor token")
				store.SaveApprovalClassification(context.Background(), proof)
			case "session":
				proof.SessionID = "different-session"
				store.memoryStore.mu.Lock()
				store.memoryStore.classifications[approvalClassificationKey(session.ID, id)] = proof
				store.memoryStore.mu.Unlock()
			case "missing":
				store.memoryStore.mu.Lock()
				delete(store.memoryStore.classifications, approvalClassificationKey(session.ID, id))
				store.memoryStore.mu.Unlock()
			}
			request := financeBrowserRequest(s, session, "POST", "/api/admin/approvals/requests/"+id+"/actions/submit", `{"expected_version":1}`)
			request.Header.Set("X-Pinvou-Approval-Action-Control-Version", "1")
			rr = httptest.NewRecorder()
			s.Handler(nil).ServeHTTP(rr, request)
			if rr.Code != tc.want {
				t.Fatalf("action status %d want %d", rr.Code, tc.want)
			}
			if (tc.want == 204 && writes.Load() != 1) || (tc.want != 204 && writes.Load() != 0) {
				t.Fatal("disabled gate reached Account mutation")
			}
		})
	}
}
func TestDisabledTokenMutationGatePreservesRevokeCleanup(t *testing.T) {
	store := newMemoryStore()
	s := testService(t, store)
	s.config.AdminModelTokenActionsEnabled = false
	session := validSession(uuid.NewString())
	store.SaveSession(context.Background(), session)
	var calls atomic.Int32
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(204) }))
	defer u.Close()
	s.config.TokenGatewayURL = parseTestURL(t, u.URL)
	for _, tc := range []struct {
		path string
		want int
	}{{"/admin/v1/commands", 404}, {"/admin/v1/credential-results/" + uuid.NewString() + "/revoke-window", 204}} {
		rr := httptest.NewRecorder()
		s.Handler(nil).ServeHTTP(rr, financeBrowserRequest(s, session, "POST", "/api/admin/token"+tc.path, `{}`))
		if rr.Code != tc.want {
			t.Fatalf("path status %d want %d", rr.Code, tc.want)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("mutation gate removed cleanup or allowed write")
	}
}
