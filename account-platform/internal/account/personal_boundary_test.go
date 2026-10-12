package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type personalBoundaryRepository struct {
	userID      uuid.UUID
	statusCalls int
	deny        bool
}

func (r *personalBoundaryRepository) Bootstrap(context.Context, identity.Principal) (Me, bool, error) {
	return Me{UserID: r.userID, Status: "PROVISIONING"}, false, nil
}
func (r *personalBoundaryRepository) GetMe(context.Context, identity.Principal) (Me, error) {
	return Me{UserID: r.userID, Status: "ACTIVE", ActiveContext: Context{Type: "PERSONAL", ContextVersion: 1, AuthorizationVersion: 1, PersonalAuthorizationVersion: 1, PlatformAuthorizationVersion: 1}}, nil
}
func (r *personalBoundaryRepository) ResolveAuthorizationContext(context.Context, identity.Principal) (AuthorizationContext, error) {
	return AuthorizationContext{UserID: r.userID, ContextType: "PERSONAL", ContextVersion: 1, AuthorizationVersion: 1}, nil
}
func (r *personalBoundaryRepository) Ping(context.Context) error { return nil }
func (r *personalBoundaryRepository) Close()                     {}
func (r *personalBoundaryRepository) Authorize(_ context.Context, _ identity.Principal, p string, _ *uuid.UUID, _ *uuid.UUID) (AuthorizationDecision, error) {
	return AuthorizationDecision{UserID: r.userID, Allowed: !r.deny && f01PermissionAllowed(p), Permission: p, UserStatus: "ACTIVE", ContextVersion: 1, AuthorizationVersion: 1}, nil
}
func (r *personalBoundaryRepository) AdminUsers(context.Context, AdminUserFilter) (AdminUserPage, error) {
	return AdminUserPage{Items: []AdminUser{}}, nil
}
func (r *personalBoundaryRepository) AdminSetUserStatus(context.Context, identity.Principal, uuid.UUID, string) error {
	r.statusCalls++
	return nil
}

func TestPersonalHandlerRejectsInvalidBodiesWithoutMutation(t *testing.T) {
	repo := &personalBoundaryRepository{userID: uuid.New()}
	auth := newMobileAuthForTest(t, &mobileAuthRepositoryFake{}, &smsProviderFake{}, &identityProvisionerFake{})
	profiles := &ProfileChangeService{store: &profileStoreFake{}, now: time.Now}
	handler := NewService(repo, nil).WithMobileAuth(auth, "private-test-service-token-123456").WithProfileChanges(profiles).Handler(func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "issuer", Subject: "subject", SessionID: "session", AuthenticatedAt: time.Now()}, nil
	})
	id := uuid.New().String()
	for _, item := range []struct{ method, path string }{
		{http.MethodPost, "/v1/me/profile-change-requests"},
		{http.MethodPost, "/v1/me/profile-change-requests/" + id + "/mobile-challenges"},
		{http.MethodPost, "/v1/me/profile-change-requests/" + id + "/actions/complete"},
		{http.MethodPost, "/v1/auth/password-resets"},
		{http.MethodPost, "/v1/me/password-changes"},
		{http.MethodPatch, "/admin/v1/users/" + id + "/status"},
	} {
		t.Run(item.path, func(t *testing.T) {
			r := httptest.NewRequest(item.method, item.path, strings.NewReader(`{"broken"`))
			r.Header.Set("Authorization", "Bearer test")
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"code":"INVALID_REQUEST"`) {
				t.Fatalf("status=%d", w.Code)
			}
		})
	}
	if repo.statusCalls != 0 {
		t.Fatal("malformed request changed user status")
	}
	for _, state := range []string{"ACTIVE", "DISABLED"} {
		r := httptest.NewRequest(http.MethodPatch, "/admin/v1/users/"+id+"/status", strings.NewReader(`{"status":"`+state+`"}`))
		r.Header.Set("Authorization", "Bearer test")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusNoContent {
			t.Fatalf("valid status response=%d", w.Code)
		}
	}
	if repo.statusCalls != 2 {
		t.Fatal("valid status did not reach repository")
	}
	r := httptest.NewRequest(http.MethodPatch, "/admin/v1/users/"+id+"/status", strings.NewReader(`{"status":"DELETED"}`))
	r.Header.Set("Authorization", "Bearer test")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest || repo.statusCalls != 2 {
		t.Fatal("direct deletion became a status mutation")
	}
	// Commerce mapping was delivered with F02. It remains protected rather
	// than being advertised as an absent future route.
	for _, path := range []string{"/v1/me/commerce-customer-mapping", "/admin/v1/approval-requests", "/v1/me/organizations"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("installed route lacked bearer protection: %s status=%d", path, response.Code)
		}
	}
	for _, item := range []struct{ method, path string }{
		{http.MethodDelete, "/admin/v1/users/" + id},
		{http.MethodPost, "/v1/me/account-closure-requests/force"},
		{http.MethodGet, "/admin/v1/registration-bonus-policy"},
	} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(item.method, item.path, nil))
		if w.Code != http.StatusNotFound {
			t.Fatalf("future route became reachable: %s", item.path)
		}
	}
	for _, item := range []struct{ method, path string }{
		{http.MethodPost, "/v1/me/account-closure-requests"},
		{http.MethodGet, "/v1/me/organizations"},
	} {
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(item.method, item.path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("installed personal route lacked bearer protection: %s status=%d", item.path, w.Code)
		}
	}
	// F02 activates this read endpoint. Its addition must preserve the
	// authentication boundary rather than the earlier wave's absent-route check.
	for _, path := range []string{"/v1/me/commerce-customer-mapping", "/admin/v1/approval-requests"} {
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("activated route accepted an unauthenticated request: %s status=%d", path, w.Code)
		}
	}
}

type activationReadinessFake struct {
	identity, wallet bool
	err              error
	userID           uuid.UUID
}

func (f *activationReadinessFake) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	f.userID = args[0].(uuid.UUID)
	return f
}

func (f *activationReadinessFake) Scan(dest ...any) error {
	if f.err != nil {
		return f.err
	}
	*dest[0].(*bool), *dest[1].(*bool) = f.identity, f.wallet
	return nil
}

func TestPersonalAdminActivationRequiresIdentityAndConfirmedWallet(t *testing.T) {
	userID := uuid.New()
	for _, item := range []struct {
		name             string
		identity, wallet bool
	}{{"unknown-identity", false, true}, {"wallet-unconfirmed", true, false}, {"neither-confirmed", false, false}, {"both-confirmed", true, true}} {
		t.Run(item.name, func(t *testing.T) {
			query := &activationReadinessFake{identity: item.identity, wallet: item.wallet}
			err := assertPersonalActivationReady(context.Background(), query, userID)
			if item.identity && item.wallet {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrAccountProvisioning) {
				t.Fatal("unconfirmed activation was allowed")
			}
			if query.userID != userID {
				t.Fatal("readiness queried another user")
			}
		})
	}
	query := &activationReadinessFake{err: context.Canceled}
	if err := assertPersonalActivationReady(context.Background(), query, userID); !errors.Is(err, context.Canceled) {
		t.Fatal("unknown readiness error was ignored")
	}
}

func TestPersonalPermissionProjectionRejectsFutureAuthority(t *testing.T) {
	input := []string{PermissionPersonalProfileRead, PermissionPersonalBillingRead, "personal.account.close.request", "personal.models.use", "platform.gift-policy.manage", "platform.approvals.read", PermissionPlatformUsersRead}
	me := Me{ActiveContext: Context{Permissions: input}, PersonalPermissions: input, PlatformPermissions: input}
	filterMePermissions(&me)
	for _, values := range [][]string{me.ActiveContext.Permissions, me.PersonalPermissions, me.PlatformPermissions} {
		if len(values) != 4 {
			t.Fatalf("unexpected delivered permissions=%v", values)
		}
	}
	repo := &PostgresRepository{}
	for _, permission := range input[3:6] {
		if _, err := repo.Authorize(context.Background(), identity.Principal{}, permission, nil, nil); !errors.Is(err, ErrForbidden) {
			t.Fatal("future permission reached storage")
		}
		if _, err := repo.AuthorizeComposite(context.Background(), identity.Principal{}, []string{permission}, nil); !errors.Is(err, ErrForbidden) {
			t.Fatal("future composite reached storage")
		}
	}
}

func TestPersonalWalletRequiresCompleteMatchingActiveACK(t *testing.T) {
	userID, walletID := uuid.New(), uuid.New()
	valid := map[string]any{"wallet_id": walletID, "owner_type": "PERSONAL", "owner_id": userID, "status": "ACTIVE", "version": 1}
	raw, _ := json.Marshal(valid)
	cases := []struct {
		name, body string
		ok         bool
	}{{"valid", string(raw), true}, {"extension", strings.TrimSuffix(string(raw), "}") + `,"extension":true}`, true}, {"missing", "{}", false}, {"null", "null", false}, {"trailing", string(raw) + ` {}`, false}, {"overflow", string(raw) + strings.Repeat(" ", 64<<10), false}}
	for _, field := range []string{"wallet_id", "owner_type", "owner_id", "status", "version"} {
		copy := map[string]any{}
		for k, v := range valid {
			copy[k] = v
		}
		delete(copy, field)
		b, _ := json.Marshal(copy)
		cases = append(cases, struct {
			name, body string
			ok         bool
		}{"missing-" + field, string(b), false})
	}
	for _, mismatch := range []struct {
		field string
		value any
	}{{"wallet_id", uuid.Nil}, {"owner_id", uuid.New()}, {"owner_type", "ORGANIZATION"}, {"status", "DISABLED"}, {"version", 0}} {
		copy := map[string]any{}
		for k, v := range valid {
			copy[k] = v
		}
		copy[mismatch.field] = mismatch.value
		b, _ := json.Marshal(copy)
		cases = append(cases, struct {
			name, body string
			ok         bool
		}{"wrong-" + mismatch.field, string(b), false})
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/internal/v1/wallets/ensure" || r.Header.Get("Idempotency-Key") != "personal-wallet:"+userID.String() {
					t.Error("wallet request facts changed")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(item.body))
			}))
			defer server.Close()
			wallets, err := NewHTTPWalletProvisioner(server.URL, "test-service-token")
			if err != nil {
				t.Fatal(err)
			}
			repo := &walletProvisioningRepositoryFake{jobs: []WalletProvisioningJob{{UserID: userID, LeaseUntil: time.Now().Add(time.Minute)}}}
			err = NewWalletProvisioningWorker(repo, wallets, time.Second).RunOnce(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if item.ok {
				if repo.completed != userID || repo.failed != uuid.Nil {
					t.Fatal("valid wallet ACK did not activate job")
				}
			} else if repo.completed != uuid.Nil || repo.failed != userID {
				t.Fatal("unconfirmed wallet ACK activated job")
			}
		})
	}
}

func TestIdentityAuthorityResponseIsCompleteAndBounded(t *testing.T) {
	for _, body := range []string{`null`, `{}`, `{"subject":""}`, `{"subject":"real-subject"}{}`, `{"subject":"real-subject"}` + strings.Repeat(" ", 64<<10)} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
		client, err := NewHTTPIdentityProvisioner(server.URL, "test-service-token")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.CreateMobileUser(context.Background(), uuid.New(), "user", "+8613800138000", "handle", "user-v1"); err == nil {
			t.Fatal("incomplete create authority accepted")
		}
		if _, _, err := client.FindMobileUserByRegistrationOperation(context.Background(), uuid.New()); err == nil {
			t.Fatal("incomplete recovery authority accepted")
		}
		server.Close()
	}
	for _, endpoint := range []string{"ftp://authority.invalid", "https://user:password@authority.invalid", "https://authority.invalid?query=x", "https://authority.invalid#fragment"} {
		if _, err := NewHTTPIdentityProvisioner(endpoint, "test-service-token"); err == nil {
			t.Fatal("unsafe identity endpoint accepted")
		}
		if _, err := NewHTTPWalletProvisioner(endpoint, "test-service-token"); err == nil {
			t.Fatal("unsafe wallet endpoint accepted")
		}
	}
}
