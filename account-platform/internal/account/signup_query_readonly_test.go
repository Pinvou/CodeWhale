package account

import (
	"github.com/google/uuid"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestF07RegistrationPublicReadOnlyQuery(t *testing.T) {
	repo := &mobileAuthRepositoryFake{}
	sms := &smsProviderFake{}
	identity := &identityProvisionerFake{}
	auth := newMobileAuthForTest(t, repo, sms, identity)
	id := uuid.New()
	token, _, err := auth.tokens.Issue(id)
	if err != nil {
		t.Fatal(err)
	}
	hash := registrationTokenHash(token)
	user := uuid.New()
	repo.operation = RegistrationOperation{ID: id, QueryTokenHash: hash[:], UserID: &user, Status: "PROVISIONING", Username: "private-username", Mobile: "+8613800138000"}
	handler := NewService(nil, nil).WithMobileAuth(auth, "controlled-workload").Handler(nil)
	call := func(method, path, key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("X-Registration-Query-Token", key)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	path := "/v1/auth/registration-operations/" + id.String()
	t.Run("matching-scoped-query-is-redacted-read-only", func(t *testing.T) {
		w := call("GET", path, token)
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), `"status":"PROVISIONING"`) {
			t.Fatalf("status=%d", w.Code)
		}
		for _, secret := range []string{"private-username", "13800138000", "query_token_hash", "password"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatal("query disclosed private signup data")
			}
		}
	})
	t.Run("missing-or-wrong-token-hidden", func(t *testing.T) {
		for _, key := range []string{"", "wrong"} {
			if w := call("GET", path, key); w.Code != 404 {
				t.Fatalf("status=%d", w.Code)
			}
		}
	})
	t.Run("foreign-operation-hidden", func(t *testing.T) {
		if w := call("GET", "/v1/auth/registration-operations/"+uuid.NewString(), token); w.Code != 404 {
			t.Fatalf("status=%d", w.Code)
		}
	})
	t.Run("expired-query-token-hidden", func(t *testing.T) {
		auth.tokens.now = func() time.Time { return time.Now().Add(49 * time.Hour) }
		if w := call("GET", path, token); w.Code != 404 {
			t.Fatalf("status=%d", w.Code)
		}
	})
	if repo.operation.Status != "PROVISIONING" || sms.sendCalls != 0 || sms.verifyCalls != 0 || identity.createCalls != 0 {
		t.Fatal("status query invoked signup effects")
	}
}
