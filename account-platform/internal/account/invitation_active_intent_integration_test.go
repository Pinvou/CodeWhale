package account

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

// Uses the installed public migration chain and the actual account_service
// repository. Absent dedicated PostgreSQL is a skip, not acceptance evidence.
func TestF06InvitationActiveIntentAndReceipts(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	f := retainedWalletFenceActors(t)
	r := f.repository(t)
	ctx := context.Background()
	user := func() (identity.Principal, uuid.UUID) {
		p := identity.Principal{Issuer: "https://identity.example.test", Subject: uuid.NewString(), SessionID: uuid.NewString(), Username: "m" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]}
		me, _, err := r.Bootstrap(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = r.pool.Exec(ctx, `UPDATE users SET status='ACTIVE' WHERE id=$1`, me.UserID); err != nil {
			t.Fatal(err)
		}
		return p, me.UserID
	}
	actor, owner := user()
	targetPrincipal, target := user()
	org, err := r.AdminCreateOrganization(ctx, actor, owner, "Controlled invitation organization", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	unitB, err := r.AdminCreateUnit(ctx, actor, org.ID, nil, "Other controlled unit", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	unit, err := r.AdminCreateUnit(ctx, actor, org.ID, nil, "Controlled unit", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.pool.Exec(ctx, `INSERT INTO role_assignments(id,user_id,role_code,created_by,created_at,starts_at) SELECT $1,$2,role.code,$2,clock_timestamp(),clock_timestamp() FROM roles role WHERE role.scope='PLATFORM' AND EXISTS(SELECT 1 FROM role_permissions p WHERE p.role_code=role.code AND p.permission_code=$3) ORDER BY role.code LIMIT 1`, uuid.New(), owner, canonicalPermissionCode(PermissionPlatformOrganizationsMembersManage)); err != nil {
		t.Fatal(err)
	}
	member, err := r.AdminAddMemberWithUnits(ctx, actor, org.ID, target, []string{"AUDITOR"}, []uuid.UUID{unit.Unit.ID}, []uuid.UUID{unit.Unit.ID}, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	invite := func(roles []string, units []uuid.UUID) Invitation {
		invitation, err := r.CreateInvitation(ctx, actor, org.ID, InvitationCreateRequest{TargetType: "LINK", Roles: roles, UnitIDs: units, ExpiresInHours: 24, IdempotencyKey: uuid.NewString()})
		if err != nil {
			t.Fatal(err)
		}
		return invitation
	}
	snapshot := func() string {
		var value string
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('memberships',(SELECT jsonb_agg(to_jsonb(m) ORDER BY id) FROM memberships m),'units',(SELECT jsonb_agg(to_jsonb(m) ORDER BY membership_id,unit_id) FROM membership_units m),'grants',(SELECT jsonb_agg(to_jsonb(b) ORDER BY id) FROM role_bindings b),'invitations',(SELECT jsonb_agg(to_jsonb(i) ORDER BY id) FROM invitations i),'commands',(SELECT jsonb_agg(to_jsonb(c) ORDER BY actor_user_id,organization_id,idempotency_key) FROM membership_commands c),'audit',(SELECT count(*) FROM audit_logs),'outbox',(SELECT count(*) FROM outbox_events),'users',(SELECT jsonb_agg(to_jsonb(u) ORDER BY id) FROM users u),'sessions',(SELECT jsonb_agg(to_jsonb(s) ORDER BY issuer,session_id) FROM user_sessions s))::text`).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	for _, tc := range []struct {
		name  string
		roles []string
		units []uuid.UUID
	}{
		{"different managed roles", []string{"ORG_ADMIN"}, nil},
		{"explicit empty ordinary units", []string{"AUDITOR"}, []uuid.UUID{}},
		{"different ordinary units", []string{"AUDITOR"}, []uuid.UUID{unitB.Unit.ID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := invite(tc.roles, tc.units)
			before := snapshot()
			if _, err := r.AcceptInvitation(ctx, targetPrincipal, i.Token, nil); !errors.Is(err, ErrConflict) || snapshot() != before {
				t.Fatal("mismatched invitation consumed or changed facts", err)
			}
			var remoteCalls atomic.Int32
			billing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				remoteCalls.Add(1)
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer billing.Close()
			lifecycle, err := NewHTTPOrganizationLifecycleClient(billing.URL, "controlled-service", "BILLING")
			if err != nil {
				t.Fatal(err)
			}
			handler := NewService(r, nil).WithOrganizationMemberLifecycle(lifecycle).Handler(func(context.Context, string) (identity.Principal, error) { return targetPrincipal, nil })
			request := httptest.NewRequest(http.MethodPost, "/v1/invitations/accept", strings.NewReader(`{"token":"`+i.Token+`"}`))
			request.Header.Set("Authorization", "Bearer controlled-bearer")
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "ACCOUNT_CONFLICT") || remoteCalls.Load() != 0 || snapshot() != before {
				t.Fatalf("HTTP conflict changed facts or invoked lifecycle: %d %s calls=%d", response.Code, response.Body.String(), remoteCalls.Load())
			}
		})
	}
	t.Run("equivalent nil units preserve independent unit administration", func(t *testing.T) {
		i := invite([]string{"AUDITOR", "MEMBER", "AUDITOR"}, nil)
		result, err := r.AcceptInvitation(ctx, targetPrincipal, i.Token, nil)
		if err != nil || !result.Replayed || result.Member.Version != member.Member.Version || !slices.Equal(result.Member.Roles, member.Member.Roles) || !slices.Equal(result.Member.UnitAdminIDs, member.Member.UnitAdminIDs) || !slices.Equal(result.Member.UnitIDs, member.Member.UnitIDs) {
			t.Fatal("equivalent intent changed grants", result, err)
		}
		current, err := r.AdminReplaceMemberRoles(ctx, actor, org.ID, target, []string{"ORG_ADMIN"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		before := snapshot()
		replay, err := r.AcceptInvitation(ctx, targetPrincipal, i.Token, nil)
		if err != nil || !replay.Replayed || !slices.Equal(replay.Member.Roles, current.Roles) || snapshot() != before {
			t.Fatal("accepted receipt regranted historical intent", replay, err)
		}
	})
	t.Run("owner implicit membership is preserved", func(t *testing.T) {
		i := invite(nil, nil)
		result, err := r.AcceptInvitation(ctx, actor, i.Token, nil)
		if err != nil || !result.Replayed || !slices.Equal(result.Member.Roles, []string{"OWNER"}) {
			t.Fatal(result, err)
		}
	})
	t.Run("explicit units and empty unscoped units match existing facts", func(t *testing.T) {
		for _, units := range [][]uuid.UUID{{unit.Unit.ID}, {}} {
			p, id := user()
			added, err := r.AdminAddMemberWithUnits(ctx, actor, org.ID, id, nil, nil, units, uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			i := invite(nil, units)
			result, err := r.AcceptInvitation(ctx, p, i.Token, nil)
			if err != nil || !result.Replayed || result.Member.Version != added.Member.Version || !slices.Equal(result.Member.UnitIDs, units) {
				t.Fatal(result, err)
			}
		}
	})
	t.Run("new and inactive members still receive invitation grants", func(t *testing.T) {
		p, id := user()
		i := invite([]string{"AUDITOR"}, []uuid.UUID{unit.Unit.ID})
		first, err := r.AcceptInvitation(ctx, p, i.Token, nil)
		if err != nil || first.Replayed || !slices.Contains(first.Member.Roles, "AUDITOR") || !slices.Equal(first.Member.UnitIDs, []uuid.UUID{unit.Unit.ID}) {
			t.Fatal(first, err)
		}
		removed, err := r.AdminRemoveMember(ctx, actor, org.ID, id, uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		i = invite([]string{"ORG_ADMIN"}, nil)
		next, err := r.AcceptInvitation(ctx, p, i.Token, nil)
		if err != nil || next.Replayed || next.Member.Version <= removed.Member.Version || !slices.Contains(next.Member.Roles, "ORG_ADMIN") || slices.Contains(next.Member.Roles, "AUDITOR") || len(next.Member.UnitIDs) != 0 {
			t.Fatal(next, err)
		}
	})
}
