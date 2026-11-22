package portal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Run with the production portal ACL, not an unrestricted Redis default user.
// The OIDC provider is local; callback, Redis scripts and lineage are real.
func TestRealRedisLoginCallbackPreservesLineageAndLogoutFence(t *testing.T) {
	ctx := context.Background()
	store := realRedisStore(t)
	var owned []string
	t.Cleanup(func() {
		for _, id := range owned {
			if err := store.client.Del(ctx, "portal:flow:"+id, sessionKey(id), sessionRootKey(id), lineageMembersKey(id), logoutMarkerKey(id), approvalWireKey(id)).Err(); err != nil {
				t.Error("owned login fixture cleanup failed")
			}
		}
	})
	var reply TokenResponse
	service, key := oidcTestService(t, store, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(reply)
	}))
	reply = TokenResponse{AccessToken: "test-access", RefreshToken: "test-refresh", TokenType: "Bearer", ExpiresIn: 60, RefreshExpiresIn: 3600}
	reply.IDToken = testIDToken(t, key, nil)
	callback := func(previous, root string) *http.Request {
		state := uuid.NewString()
		owned = append(owned, state)
		flow := LoginFlow{Verifier: "test-verifier", Nonce: "nonce-test", ReturnTo: "/", PreviousSession: previous, LineageRoot: root, ExpiresAt: time.Now().Add(time.Minute)}
		if err := store.SaveFlow(ctx, state, flow); err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodGet, "/auth/callback?state="+state+"&code=test-code", nil)
		r.AddCookie(&http.Cookie{Name: service.flowCookieName(), Value: state})
		return r
	}
	commit := func(request *http.Request) Session {
		response := httptest.NewRecorder()
		service.Handler(nil).ServeHTTP(response, request)
		if response.Code != http.StatusFound || response.Header().Get("Location") != "/" {
			t.Fatalf("valid signed callback did not create session: status=%d body=%s", response.Code, response.Body.String())
		}
		var id string
		for _, cookie := range response.Result().Cookies() {
			if cookie.Name == service.sessionCookieName() {
				id = cookie.Value
			}
		}
		if id == "" {
			t.Fatal("successful callback did not deliver session cookie")
		}
		owned = append(owned, id)
		session, err := store.GetSession(ctx, id)
		if err != nil || session.Subject != "user-test" || !session.ApprovalV2Eligible || session.OIDCClientID != "portal" {
			t.Fatal("callback session facts were not persisted")
		}
		ttl, err := store.client.PTTL(ctx, sessionRootKey(id)).Result()
		if err != nil || ttl <= 0 || ttl > time.Hour {
			t.Fatal("callback lineage mapping has no bounded positive lifetime")
		}
		return session
	}
	first := commit(callback("", ""))
	second := commit(callback(first.ID, first.LineageRoot))
	if first.LineageRoot != first.ID || second.LineageRoot != first.ID {
		t.Fatal("replacement callback lost original lineage")
	}
	late := callback(second.ID, second.LineageRoot)
	logout := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(logout, logoutRequest(service, second))
	if logout.Code != http.StatusNoContent {
		t.Fatalf("real Redis logout failed: %d", logout.Code)
	}
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, late)
	if response.Code != http.StatusConflict {
		t.Fatalf("late callback revived logged-out lineage: %d", response.Code)
	}
	if _, err := store.GetSession(ctx, second.ID); err != ErrSessionNotFound {
		t.Fatal("logout left the browser session accessible")
	}
}
