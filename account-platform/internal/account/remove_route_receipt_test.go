package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

// Controlled repository implements only the real router's used methods. Nil
// embedded production repository satisfies unused organization capabilities;
// any accidental access to those methods fails rather than simulating storage.
// Billing is controlled HTTP, not the production repository or PostgreSQL.
type removeRouteRepository struct {
	*PostgresRepository
	t                  *testing.T
	principal          identity.Principal
	result             MembershipMutationResult
	forbidden          bool
	calls              int
	keys               []string
	admin              bool
	expectedPermission string
}

func (f *removeRouteRepository) Bootstrap(context.Context, identity.Principal) (Me, bool, error) {
	panic("unexpected Bootstrap")
}
func (f *removeRouteRepository) GetMe(_ context.Context, p identity.Principal) (Me, error) {
	if !reflect.DeepEqual(p, f.principal) {
		f.t.Error("GetMe identity changed")
	}
	return Me{}, nil
}

func (f *removeRouteRepository) GetAuthorizationVersions(context.Context, identity.Principal) (AuthorizationVersions, error) {
	return AuthorizationVersions{}, nil
}
func (f *removeRouteRepository) ResolveAuthorizationContext(context.Context, identity.Principal) (AuthorizationContext, error) {
	panic("unexpected context resolver")
}
func (f *removeRouteRepository) Ping(context.Context) error { return nil }
func (f *removeRouteRepository) Close()                     {}
func (f *removeRouteRepository) Authorize(_ context.Context, p identity.Principal, permission string, org, unit *uuid.UUID) (AuthorizationDecision, error) {
	if !reflect.DeepEqual(p, f.principal) || permission != f.expectedPermission || org != nil || unit != nil {
		f.t.Error("admin route permission or identity changed")
	}
	return AuthorizationDecision{Allowed: true, Permission: permission}, nil
}
func (f *removeRouteRepository) remove(p identity.Principal, org, user uuid.UUID, key string, admin bool) (MembershipMutationResult, error) {
	f.calls++
	f.keys = append(f.keys, key)
	if !reflect.DeepEqual(p, f.principal) || org != f.result.OrganizationID || user != f.result.Member.UserID || key != "original-remove-operation-key" || admin != f.admin {
		f.t.Error("remove caller identity/target/key changed")
	}
	if f.forbidden {
		return MembershipMutationResult{}, ErrForbidden
	}
	return f.result, nil
}
func (f *removeRouteRepository) RevokeMembership(_ context.Context, p identity.Principal, org, user uuid.UUID, key string) (MembershipMutationResult, error) {
	return f.remove(p, org, user, key, false)
}
func (f *removeRouteRepository) AdminRemoveMember(_ context.Context, p identity.Principal, org, user uuid.UUID, key string) (MembershipMutationResult, error) {
	return f.remove(p, org, user, key, true)
}

func TestMembershipMutationRoutesProjectCurrentReceipt(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	for _, route := range []struct {
		name, prefix string
		admin        bool
		accept       bool
	}{{"ordinary", "/v1", false, false}, {"administrator", "/admin/v1", true, false}, {"invitation", "/v1", false, true}} {
		t.Run(route.name, func(t *testing.T) {
			for _, tc := range []struct {
				name, status                     string
				replayed, forbidden, unavailable bool
			}{
				{name: "active-readd-replay", status: "ACTIVE", replayed: true},
				{name: "revoked-replay", status: "REVOKED", replayed: true},
				{name: "suspended-replay", status: "SUSPENDED", replayed: true},
				{name: "first-revoked", status: "REVOKED"},
				{name: "repository-forbidden", status: "REVOKED", forbidden: true},
				{name: "projection-unavailable-same-key-retry", status: "ACTIVE", replayed: true, unavailable: true},
			} {
				if route.accept && tc.name == "first-revoked" {
					tc.name = "fresh-active"
					tc.status = "ACTIVE"
				}
				t.Run(tc.name, func(t *testing.T) {
					org, user := uuid.New(), uuid.New()
					principal := identity.Principal{Issuer: "https://identity.example.test", Subject: uuid.NewString(), SessionID: uuid.NewString()}
					result := MembershipMutationResult{OrganizationID: org, Member: Member{UserID: user, Status: tc.status, Version: 17, MembershipID: uuid.New(), Roles: []string{"MEMBER"}, UnitIDs: []uuid.UUID{uuid.New()}}, Replayed: tc.replayed}
					repo := &removeRouteRepository{t: t, principal: principal, result: result, forbidden: tc.forbidden, admin: route.admin}
					repo.expectedPermission = PermissionPlatformOrganizationsMembersManage
					if route.accept {
						repo.expectedPermission = PermissionPersonalInvitationsAccept
					}
					var mu sync.Mutex
					var projections []OrganizationMemberLifecycleState
					billing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						mu.Lock()
						defer mu.Unlock()
						expectedPath := "/internal/v1/organizations/" + org.String() + "/members/" + user.String() + "/lifecycle"
						if r.Method != http.MethodPut || r.URL.Path != expectedPath || r.Header.Get("Authorization") != "Bearer controlled-service" || r.Header.Get("Content-Type") != "application/json" {
							t.Error("lifecycle HTTP target/method/workload credential changed")
						}
						var body struct {
							Status  string `json:"status"`
							Version int64  `json:"membership_version"`
						}
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Error(err)
							w.WriteHeader(400)
							return
						}
						projections = append(projections, OrganizationMemberLifecycleState{OrganizationID: org, UserID: user, Status: body.Status, MembershipVersion: body.Version})
						if tc.unavailable && len(projections) == 1 {
							w.WriteHeader(503)
							return
						}
						// Mirrors existing Billing same-version/different-status rejection. A
						// replay must project authoritative current facts, never historical REVOKED.
						if body.Status != tc.status || body.Version != result.Member.Version {
							w.WriteHeader(409)
							return
						}
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(OrganizationMemberLifecycleState{OrganizationID: org, UserID: user, Status: tc.status, MembershipVersion: result.Member.Version})
					}))
					defer billing.Close()
					lifecycle, err := NewHTTPOrganizationLifecycleClient(billing.URL, "controlled-service", "BILLING")
					if err != nil {
						t.Fatal(err)
					}
					handler := NewService(repo, nil).WithOrganizationMemberLifecycle(lifecycle).Handler(func(_ context.Context, token string) (identity.Principal, error) {
						if token != "controlled-bearer" {
							return identity.Principal{}, errors.New("unexpected bearer")
						}
						return principal, nil
					})
					send := func() *httptest.ResponseRecorder {
						req := httptest.NewRequest(http.MethodDelete, route.prefix+"/organizations/"+org.String()+"/members/"+user.String(), nil)
						if route.accept {
							req = httptest.NewRequest(http.MethodPost, "/v1/invitations/accept", strings.NewReader(`{"token":"original-invitation-token"}`))
							req.Header.Set("Content-Type", "application/json")
						}
						req.Header.Set("Authorization", "Bearer controlled-bearer")
						req.Header.Set("Idempotency-Key", "original-remove-operation-key")
						req.Header.Set("X-Request-ID", "controlled-remove-request")
						out := httptest.NewRecorder()
						handler.ServeHTTP(out, req)
						return out
					}
					retryHint := "same idempotency key"
					if route.accept {
						retryHint = "same invitation reference"
					}
					out := send()
					if tc.forbidden {
						if out.Code != http.StatusForbidden {
							t.Fatalf("repository denial=%d body=%s", out.Code, out.Body.String())
						}
					} else {
						if tc.unavailable {
							if out.Code != 503 || !strings.Contains(out.Body.String(), "ORGANIZATION_MEMBER_LIFECYCLE_UNAVAILABLE") || !strings.Contains(out.Body.String(), retryHint) || strings.Contains(out.Body.String(), "was revoked locally") {
								t.Fatalf("recoverable current-state projection error=%d body=%s", out.Code, out.Body.String())
							}
							out = send()
						}
						if out.Code != 200 {
							t.Fatalf("current membership %s projection=%d body=%s", tc.status, out.Code, out.Body.String())
						}
						var got MembershipMutationResult
						if err := json.Unmarshal(out.Body.Bytes(), &got); err != nil {
							t.Fatal(err)
						}
						if !reflect.DeepEqual(got, result) {
							t.Fatalf("response changed current status/version/target/replay facts: %+v", got)
						}
					}
					mu.Lock()
					got := append([]OrganizationMemberLifecycleState(nil), projections...)
					mu.Unlock()
					expectedCalls := 1
					if tc.unavailable {
						expectedCalls = 2
					}
					for _, key := range repo.keys {
						expectedKey := "original-remove-operation-key"
						if route.accept {
							expectedKey = "original-invitation-token"
						}
						if key != expectedKey {
							t.Fatal("retry replaced original invocation")
						}
					}
					if repo.calls != expectedCalls || len(repo.keys) != expectedCalls {
						t.Fatal("unexpected repository retry count")
					}
					if tc.forbidden {
						if len(got) != 0 {
							t.Fatal("repository denial projected lifecycle")
						}
						return
					}
					if len(got) != expectedCalls {
						t.Fatal("unexpected lifecycle projection count")
					}
					for _, projection := range got {
						if projection.OrganizationID != org || projection.UserID != user || projection.Status != tc.status || projection.MembershipVersion != result.Member.Version {
							t.Fatalf("projected historical or foreign receipt: %+v", projection)
						}
					}
				})
			}
		})
	}
}

func (f *removeRouteRepository) AcceptInvitation(_ context.Context, p identity.Principal, token string, invitationID *uuid.UUID) (MembershipMutationResult, error) {
	f.calls++
	f.keys = append(f.keys, token)
	if !reflect.DeepEqual(p, f.principal) || token != "original-invitation-token" || invitationID != nil {
		f.t.Error("invitation retry token or identity changed")
	}
	if f.forbidden {
		return MembershipMutationResult{}, ErrForbidden
	}
	return f.result, nil
}

var _ Repository = (*removeRouteRepository)(nil)
var _ organizationRepository = (*removeRouteRepository)(nil)
var _ organizationAdminRepository = (*removeRouteRepository)(nil)
