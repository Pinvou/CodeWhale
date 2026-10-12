package billing

import (
	"context"
	"encoding/json"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type signupPolicyRepository struct{ writes int }

func (*signupPolicyRepository) ActiveRegistrationBonusPolicy(context.Context) (RegistrationBonusPolicy, error) {
	return RegistrationBonusPolicy{Revision: 1, ValidityDays: 30}, nil
}
func (*signupPolicyRepository) ReplayRegistrationBonusPolicy(context.Context, uuid.UUID, bool, int64, int, int64, string, string) (RegistrationBonusPolicy, bool, error) {
	return RegistrationBonusPolicy{}, false, nil
}
func (r *signupPolicyRepository) UpdateRegistrationBonusPolicy(_ context.Context, _ uuid.UUID, enabled bool, amount int64, days int, _ int64, _ string, key string) (RegistrationBonusPolicy, bool, error) {
	if key != "signup-policy-route-key" {
		return RegistrationBonusPolicy{}, false, ErrRegistrationBonusInvalid
	}
	r.writes++
	return RegistrationBonusPolicy{Revision: 2, Enabled: enabled, AmountMicrocredits: amount, ValidityDays: days}, false, nil
}
func (*signupPolicyRepository) ReplayRegistrationBonusProvision(context.Context, RegistrationBonusProvisionRequest) (RegistrationBonusReceipt, bool, error) {
	return RegistrationBonusReceipt{}, false, nil
}
func (*signupPolicyRepository) ProvisionRegistrationBonus(context.Context, RegistrationBonusProvisionRequest) (RegistrationBonusReceipt, bool, error) {
	panic("admin policy must not provision a gift")
}

type signupReady struct{}

func (signupReady) Readiness(context.Context) (RegistrationBonusEligibilityReadiness, error) {
	now := time.Now().UTC()
	return RegistrationBonusEligibilityReadiness{Status: "READY", FingerprintKeyID: "controlled-key", CompletedAt: &now}, nil
}
func (signupReady) RequireReady(context.Context) error { return nil }
func TestF07PolicyInstalledRoutes(t *testing.T) {
	actor := uuid.New()
	allowed := true
	seen := ""
	authTime := time.Now().UTC()
	authority := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Permission string `json:"permission"`
		}
		if r.URL.Path != "/internal/v1/authorization/check" || r.Header.Get("Authorization") != "Bearer controlled-workload" {
			t.Error("wrong authority path or credential")
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid authority request")
		}
		seen = body.Permission
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": allowed, "user_id": actor})
	}))
	defer authority.Close()
	authorizer, err := authorization.NewClient(authority.URL, "controlled-workload")
	if err != nil {
		t.Fatal(err)
	}
	repo := &signupPolicyRepository{}
	service := NewService(nil).WithAuthorizer(authorizer).WithRegistrationBonusEligibilityVerifier(signupReady{})
	service.registrationBonuses = repo
	verify := identity.VerifyFunc(func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "https://controlled.identity.test", Subject: actor.String(), AuthenticatedAt: authTime}, nil
	})
	handler := service.Handler("controlled-workload", verify)
	call := func(method, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/admin/v1/registration-bonus-policy", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer controlled-human")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", "signup-policy-route-key")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	payload := `{"enabled":true,"amount_points":123,"validity_days":30,"expected_revision":1,"reason":"controlled policy update"}`
	t.Run("read-real-route-and-read-permission", func(t *testing.T) {
		w := call("GET", "")
		if w.Code != 200 || seen != "platform.registration-bonus.read" || !strings.Contains(w.Body.String(), `"eligibility_status":"READY"`) {
			t.Fatalf("GET status=%d permission=%s", w.Code, seen)
		}
		if repo.writes != 0 {
			t.Fatal("read mutated policy")
		}
	})
	t.Run("update-real-route-recent-auth-and-audit-only", func(t *testing.T) {
		w := call("PUT", payload)
		if w.Code != 200 || seen != "platform.registration-bonus.manage" || w.Header().Get("X-Command-Type") != "BILLING_REGISTRATION_BONUS_POLICY_UPDATE" || repo.writes != 1 || !strings.Contains(w.Body.String(), `"amount_microcredits":1230000`) {
			t.Fatalf("PUT status=%d writes=%d policy=%s", w.Code, repo.writes, w.Header().Get("X-Command-Type"))
		}
	})
	t.Run("permission-denied-before-mutation", func(t *testing.T) {
		allowed = false
		defer func() { allowed = true }()
		n := repo.writes
		if w := call("PUT", payload); w.Code != 403 || repo.writes != n {
			t.Fatalf("denied status=%d", w.Code)
		}
	})
	t.Run("stale-auth-before-mutation", func(t *testing.T) {
		authTime = time.Now().Add(-20 * time.Minute)
		defer func() { authTime = time.Now() }()
		n := repo.writes
		if w := call("PUT", payload); w.Code != 401 || repo.writes != n {
			t.Fatalf("old auth status=%d", w.Code)
		}
	})
	t.Run("malformed-body-does-not-mutate", func(t *testing.T) {
		n := repo.writes
		if w := call("PUT", "{"); w.Code != 400 || repo.writes != n {
			t.Fatalf("malformed status=%d", w.Code)
		}
	})
	t.Run("no-public-policy-write", func(t *testing.T) {
		if w := call("POST", payload); w.Code != 404 && w.Code != 405 {
			t.Fatalf("POST status=%d", w.Code)
		}
	})
}
