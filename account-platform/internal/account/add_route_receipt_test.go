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
type addRouteRepository struct {
	*PostgresRepository
	t                  *testing.T
	units              []uuid.UUID
	principal          identity.Principal
	result             MembershipMutationResult
	forbidden          bool
	calls              int
	keys               []string
	admin              bool
	expectedPermission string
}

func (f *addRouteRepository) Bootstrap(context.Context, identity.Principal) (Me, bool, error) {
	panic("unexpected Bootstrap")
}
func (f *addRouteRepository) GetMe(_ context.Context, p identity.Principal) (Me, error) {
	if !reflect.DeepEqual(p, f.principal) {
		f.t.Error("GetMe identity changed")
	}
	return Me{}, nil
}

func (f *addRouteRepository) GetAuthorizationVersions(context.Context, identity.Principal) (AuthorizationVersions, error) {
	return AuthorizationVersions{}, nil
}
func (f *addRouteRepository) ResolveAuthorizationContext(context.Context, identity.Principal) (AuthorizationContext, error) {
	panic("unexpected context resolver")
}
func (f *addRouteRepository) Ping(context.Context) error { return nil }
func (f *addRouteRepository) Close()                     {}
func (f *addRouteRepository) Authorize(_ context.Context, p identity.Principal, permission string, org, unit *uuid.UUID) (AuthorizationDecision, error) {
	if !reflect.DeepEqual(p, f.principal) || permission != f.expectedPermission || org != nil || unit != nil {
		f.t.Error("admin route permission or identity changed")
	}
	return AuthorizationDecision{Allowed: true, Permission: permission}, nil
}
func (f *addRouteRepository) add(p identity.Principal, org, user uuid.UUID, roles []string, unitAdmin, units []uuid.UUID, key string) (MembershipMutationResult, error) {
	f.calls++
	f.keys = append(f.keys, key)
	if !reflect.DeepEqual(p, f.principal) || org != f.result.OrganizationID || user != f.result.Member.UserID || key != "original-add-operation-key" || !reflect.DeepEqual(roles, []string{"MEMBER"}) || len(unitAdmin) != 0 || !reflect.DeepEqual(units, f.units) {
		f.t.Error("ADD original actor/organization/target/roles/units/key changed")
	}
	if f.forbidden {
		return MembershipMutationResult{}, ErrForbidden
	}
	return f.result, nil
}
func (f *addRouteRepository) AdminAddMember(_ context.Context, p identity.Principal, org, user uuid.UUID, roles []string, unitAdmin []uuid.UUID, key string) (MembershipMutationResult, error) {
	return f.add(p, org, user, roles, unitAdmin, nil, key)
}
func (f *addRouteRepository) AdminAddMemberWithUnits(_ context.Context, p identity.Principal, org, user uuid.UUID, roles []string, unitAdmin, units []uuid.UUID, key string) (MembershipMutationResult, error) {
	return f.add(p, org, user, roles, unitAdmin, units, key)
}

var _ Repository = (*addRouteRepository)(nil)
var _ organizationAdminRepository = (*addRouteRepository)(nil)

func TestAdminAddRouteProjectsCurrentReceipt(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	for _, explicit := range []bool{false, true} {
		name := "legacy"
		if explicit {
			name = "explicit-units"
		}
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct {
				name, status                     string
				replayed, forbidden, unavailable bool
			}{
				{name: "current-revoked-replay", status: "REVOKED", replayed: true},
				{name: "current-suspended-replay", status: "SUSPENDED", replayed: true},
				{name: "current-active-readd-replay", status: "ACTIVE", replayed: true},
				{name: "fresh-active", status: "ACTIVE"},
				{name: "repository-forbidden", status: "REVOKED", forbidden: true},
				{name: "projection-unavailable-same-key", status: "REVOKED", replayed: true, unavailable: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					org, user := uuid.New(), uuid.New()
					principal := identity.Principal{Issuer: "https://identity.example.test", Subject: uuid.NewString(), SessionID: uuid.NewString()}
					result := MembershipMutationResult{OrganizationID: org, Member: Member{MembershipID: uuid.New(), UserID: user, Status: tc.status, Version: 17, Roles: []string{"MEMBER"}}, Replayed: tc.replayed}
					var inputUnits []uuid.UUID
					if explicit {
						inputUnits = []uuid.UUID{uuid.New()}
					}
					repo := &addRouteRepository{t: t, principal: principal, result: result, forbidden: tc.forbidden, expectedPermission: PermissionPlatformOrganizationsMembersManage, units: inputUnits}
					var mu sync.Mutex
					var projections []OrganizationMemberLifecycleState
					billing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						mu.Lock()
						defer mu.Unlock()
						if r.Method != http.MethodPut || r.URL.Path != "/internal/v1/organizations/"+org.String()+"/members/"+user.String()+"/lifecycle" || r.Header.Get("Authorization") != "Bearer controlled-service" {
							t.Error("actual HTTP lifecycle target/method/service identity changed")
						}
						var body struct {
							Status  string `json:"status"`
							Version int64  `json:"membership_version"`
						}
						if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
							t.Error(e)
							w.WriteHeader(400)
							return
						}
						projections = append(projections, OrganizationMemberLifecycleState{OrganizationID: org, UserID: user, Status: body.Status, MembershipVersion: body.Version})
						if tc.unavailable && len(projections) == 1 {
							w.WriteHeader(503)
							return
						}
						if body.Status != tc.status || body.Version != result.Member.Version {
							w.WriteHeader(409)
							return
						}
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(OrganizationMemberLifecycleState{OrganizationID: org, UserID: user, Status: tc.status, MembershipVersion: result.Member.Version})
					}))
					defer billing.Close()
					lifecycle, e := NewHTTPOrganizationLifecycleClient(billing.URL, "controlled-service", "BILLING")
					if e != nil {
						t.Fatal(e)
					}
					handler := NewService(repo, nil).WithOrganizationMemberLifecycle(lifecycle).Handler(func(_ context.Context, token string) (identity.Principal, error) {
						if token != "controlled-bearer" {
							return identity.Principal{}, errors.New("unexpected bearer")
						}
						return principal, nil
					})
					payload := map[string]any{"user_id": user, "roles": []string{"MEMBER"}, "idempotency_key": "original-add-operation-key"}
					if explicit {
						payload["unit_ids"] = inputUnits
					}
					body, e := json.Marshal(payload)
					if e != nil {
						t.Fatal(e)
					}
					send := func() *httptest.ResponseRecorder {
						req := httptest.NewRequest(http.MethodPost, "/admin/v1/organizations/"+org.String()+"/members", strings.NewReader(string(body)))
						req.Header.Set("Authorization", "Bearer controlled-bearer")
						req.Header.Set("Content-Type", "application/json")
						req.Header.Set("Idempotency-Key", "original-add-operation-key")
						out := httptest.NewRecorder()
						handler.ServeHTTP(out, req)
						return out
					}
					out := send()
					if tc.forbidden {
						if out.Code != 403 {
							t.Fatalf("repo forbidden became %d %s", out.Code, out.Body.String())
						}
					} else {
						if tc.unavailable {
							if out.Code != 503 || !strings.Contains(out.Body.String(), "same idempotency key") || strings.Contains(out.Body.String(), "was added locally") {
								t.Fatalf("current ADD retry description %d %s", out.Code, out.Body.String())
							}
							out = send()
						}
						want := 201
						if tc.replayed {
							want = 200
						}
						if out.Code != want {
							t.Fatalf("current %s ADD projection=%d body=%s", tc.status, out.Code, out.Body.String())
						}
						var got MembershipMutationResult
						if e := json.Unmarshal(out.Body.Bytes(), &got); e != nil {
							t.Fatal(e)
						}
						if !reflect.DeepEqual(got, result) {
							t.Fatal("current response/status/version/replayed changed", got)
						}
					}
					mu.Lock()
					got := append([]OrganizationMemberLifecycleState(nil), projections...)
					mu.Unlock()
					wantCalls := 1
					if tc.unavailable {
						wantCalls = 2
					}
					if repo.calls != wantCalls || len(repo.keys) != wantCalls {
						t.Fatal("ADD retry changed count")
					}
					for _, key := range repo.keys {
						if key != "original-add-operation-key" {
							t.Fatal("replaced ADD logical key")
						}
					}
					if tc.forbidden {
						if len(got) != 0 {
							t.Fatal("denied ADD projected")
						}
						return
					}
					if len(got) != wantCalls {
						t.Fatal("projection count changed")
					}
					for _, v := range got {
						if v.OrganizationID != org || v.UserID != user || v.Status != tc.status || v.MembershipVersion != result.Member.Version {
							t.Fatal("historical or foreign ADD receipt projected", v)
						}
					}
				})
			}
		})
	}
}
